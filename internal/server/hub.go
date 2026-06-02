package server

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/protobuf/proto"
)

// hub is the server's single-goroutine owner of the broadcast model: the
// (non-authoritative, in P2) persisted-state cache, the live-runtime sidecar
// map, and the set of Watch subscribers. Everything that reads or mutates that
// state runs as a closure on the `in` channel, so there are no model mutexes and
// ordering is deterministic — in particular a subscriber's registration and its
// initial snapshot happen in ONE turn (the include_initial_state guarantee).
//
// Fan-out is non-blocking and conflating: each subscriber holds a tiny pending
// buffer (newest State wins; runtime merged by key) plus a size-1 notify
// channel. A slow client therefore never stalls the hub loop or other
// subscribers — it just coalesces missed updates into the newest snapshot.
type hub struct {
	in   chan func(*hub)
	done chan struct{} // closed when run() returns

	st      *fleetgrpc.State                      // newest persisted snapshot (owned by the loop)
	runtime map[string]*fleetgrpc.InstanceRuntime // key=fleet/instance (owned by the loop; merged in place)
	subs    map[*subscriber]struct{}
	agent   *agentTracker // stateful agent-activity detection (owned by the loop)

	// runtimeWanted is true while at least one subscriber asked for runtime;
	// the runtime pollers read it lock-free to gate their expensive work.
	runtimeWanted atomic.Bool
	// runtimeEdge fires (non-blocking) when runtimeWanted flips false->true, so
	// the live-status poller can do an immediate pass instead of waiting a tick.
	runtimeEdge chan struct{}

	// reprovisioning dedupes in-flight Claude-hook reinstall attempts per
	// containerID (the capture loop fires every few seconds; a reinstall on a
	// slow backend can outlast one tick). Accessed only from the stats/activity
	// poller goroutine + its reinstall goroutines, so its own locking suffices.
	reprovisioning sync.Map
}

func newHub() *hub {
	return &hub{
		in:          make(chan func(*hub), 64),
		done:        make(chan struct{}),
		st:          &fleetgrpc.State{},
		runtime:     make(map[string]*fleetgrpc.InstanceRuntime),
		subs:        make(map[*subscriber]struct{}),
		agent:       newAgentTracker(),
		runtimeEdge: make(chan struct{}, 1),
	}
}

// run is the actor loop. It owns all hub mutable state; only closures posted to
// `in` may touch it. It exits when ctx is cancelled (server shutdown), closing
// `done` so blocked posters and Watch pumps unwind.
func (h *hub) run(ctx context.Context) {
	defer close(h.done)
	for {
		select {
		case fn := <-h.in:
			fn(h)
		case <-ctx.Done():
			return
		}
	}
}

// post runs fn on the hub loop. It returns false if the hub has stopped (so
// callers don't block forever during shutdown).
func (h *hub) post(fn func(*hub)) bool {
	select {
	case h.in <- fn:
		return true
	case <-h.done:
		return false
	}
}

// --- loop-only methods (must be called from inside a posted closure) ---

func (h *hub) addSub(sub *subscriber) {
	h.subs[sub] = struct{}{}
	h.recomputeRuntimeWanted()
}

func (h *hub) removeSub(sub *subscriber) {
	delete(h.subs, sub)
	h.recomputeRuntimeWanted()
}

// recomputeRuntimeWanted derives the gate from the live subscriber SET (not a
// hand-maintained counter, which is prone to double-decrement on reconnect),
// and fires the false->true edge.
func (h *hub) recomputeRuntimeWanted() {
	want := false
	for s := range h.subs {
		if s.wantRuntime {
			want = true
			break
		}
	}
	prev := h.runtimeWanted.Swap(want)
	if want && !prev {
		select {
		case h.runtimeEdge <- struct{}{}:
		default:
		}
	}
}

// setState replaces the persisted snapshot and broadcasts it if it actually
// changed. h.st is replaced wholesale (never mutated in place), so the object
// handed to subscribers is safe to share without cloning.
func (h *hub) setState(st *fleetgrpc.State) {
	if proto.Equal(h.st, st) {
		return
	}
	h.st = st
	for sub := range h.subs {
		sub.enqueueState(st)
	}
}

// broadcastRuntime fans changed runtime entries out to runtime subscribers. The
// entries are cloned because h.runtime values are mutated in place by the
// pollers, and a slow subscriber may still hold a reference to a prior push.
func (h *hub) broadcastRuntime(changed []*fleetgrpc.InstanceRuntime) {
	if len(changed) == 0 {
		return
	}
	for sub := range h.subs {
		if !sub.wantRuntime {
			continue
		}
		cloned := make([]*fleetgrpc.InstanceRuntime, len(changed))
		for i, r := range changed {
			cloned[i] = cloneRuntime(r)
		}
		sub.enqueueRuntime(cloned)
	}
}

func cloneRuntime(r *fleetgrpc.InstanceRuntime) *fleetgrpc.InstanceRuntime {
	return proto.Clone(r).(*fleetgrpc.InstanceRuntime)
}

func runtimeKey(fleetName, instance string) string { return fleetName + "/" + instance }

// subscriber is one Watch stream's conflating buffer. pendingState keeps the
// newest State (older ones are dropped — a full snapshot supersedes them);
// pendingRuntime merges by key so a stats-only update and a live-status-only
// update for the same instance both survive. notify is a size-1 signal; the
// per-subscriber pump (watch.go) drains and sends. enqueue NEVER holds mu across
// a network send.
type subscriber struct {
	wantRuntime bool

	mu             sync.Mutex
	pendingState   *fleetgrpc.State
	pendingRuntime map[string]*fleetgrpc.InstanceRuntime

	notify chan struct{}
}

func newSubscriber(wantRuntime bool) *subscriber {
	return &subscriber{
		wantRuntime:    wantRuntime,
		pendingRuntime: make(map[string]*fleetgrpc.InstanceRuntime),
		notify:         make(chan struct{}, 1),
	}
}

func (s *subscriber) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *subscriber) enqueueState(st *fleetgrpc.State) {
	s.mu.Lock()
	s.pendingState = st
	s.mu.Unlock()
	s.signal()
}

func (s *subscriber) enqueueRuntime(items []*fleetgrpc.InstanceRuntime) {
	s.mu.Lock()
	for _, r := range items {
		s.pendingRuntime[runtimeKey(r.GetFleet(), r.GetInstance())] = r
	}
	s.mu.Unlock()
	s.signal()
}

// drain takes the pending state + runtime out of the buffer for sending.
func (s *subscriber) drain() (*fleetgrpc.State, []*fleetgrpc.InstanceRuntime) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.pendingState
	s.pendingState = nil
	var rt []*fleetgrpc.InstanceRuntime
	if len(s.pendingRuntime) > 0 {
		rt = make([]*fleetgrpc.InstanceRuntime, 0, len(s.pendingRuntime))
		for _, r := range s.pendingRuntime {
			rt = append(rt, r)
		}
		s.pendingRuntime = make(map[string]*fleetgrpc.InstanceRuntime)
	}
	return st, rt
}
