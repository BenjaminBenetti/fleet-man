package server

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func TestForEachBoundedVisitsEveryIndexOnceWithinTheLimit(t *testing.T) {
	for _, tc := range []struct{ n, limit int }{{0, 8}, {1, 8}, {3, 8}, {20, 8}, {20, 1}, {5, 0}} {
		var inflight, peak atomic.Int32
		visits := make([]atomic.Int32, tc.n)
		forEachBounded(tc.n, tc.limit, func(i int) {
			raisePeak(&peak, inflight.Add(1))
			time.Sleep(time.Millisecond)
			visits[i].Add(1)
			inflight.Add(-1)
		})
		for i := range visits {
			if got := visits[i].Load(); got != 1 {
				t.Fatalf("n=%d limit=%d: index %d visited %d times, want once", tc.n, tc.limit, i, got)
			}
		}
		if limit := max(tc.limit, 1); int(peak.Load()) > limit {
			t.Fatalf("n=%d limit=%d: %d calls ran at once", tc.n, tc.limit, peak.Load())
		}
	}
}

// raisePeak records now as the new peak when it is higher.
func raisePeak(peak *atomic.Int32, now int32) {
	for {
		p := peak.Load()
		if now <= p || peak.CompareAndSwap(p, now) {
			return
		}
	}
}

// gatedBackend blocks every poll call until released, counting how many are in
// flight at once. Calls other than the poller's panic on the nil embedded
// interface.
type gatedBackend struct {
	backend.Backend
	release  chan struct{}
	once     sync.Once
	arrived  chan string
	inflight atomic.Int32
	peak     atomic.Int32
}

// newGatedBackend returns a gate for up to n polls; the test's cleanup releases
// it so a failed assertion never leaves a pass blocked.
func newGatedBackend(t *testing.T, n int) *gatedBackend {
	g := &gatedBackend{release: make(chan struct{}), arrived: make(chan string, n)}
	t.Cleanup(g.releaseAll)
	return g
}

func (g *gatedBackend) releaseAll() { g.once.Do(func() { close(g.release) }) }

func (g *gatedBackend) gate(containerID string) {
	raisePeak(&g.peak, g.inflight.Add(1))
	g.arrived <- containerID
	<-g.release
	g.inflight.Add(-1)
}

func (g *gatedBackend) ListSessions(containerID string) string {
	g.gate(containerID)
	return "main-" + containerID + ":1:0\n"
}

func (g *gatedBackend) CaptureAllSessions(containerID string) backend.AllSessions {
	g.gate(containerID)
	return backend.AllSessions{OK: true}
}

func (g *gatedBackend) AgentToolProbe(string) (string, bool) { return "", false }

func (g *gatedBackend) Stats(ids []string) (map[string]*backend.ContainerStats, error) {
	return map[string]*backend.ContainerStats{}, nil
}

// seedRunning saves n running devcontainer instances and points the hub's
// backend cache for each at b, so the passes poll through b.
func seedRunning(t *testing.T, h *hub, n int, b backend.Backend) {
	t.Helper()
	isolateFleetDir(t)
	f := &fleet.Fleet{Name: "alpha"}
	h.backends = make(map[string]backend.Backend)
	for i := range n {
		inst := &fleet.Instance{Name: fmt.Sprintf("i%02d", i), Status: fleet.StatusRunning, Backend: fleet.BackendDevcontainer, ContainerID: fmt.Sprintf("c%02d", i)}
		f.Instances = append(f.Instances, inst)
		h.backends[backendCacheKey(inst)] = b
	}
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// assertBoundedPass runs pass with n instances whose polls block, and checks
// that exactly pollerWorkers polls run at once: more than one (the pass is no
// longer serial) and never more than the bound.
func assertBoundedPass(t *testing.T, g *gatedBackend, n int, pass func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { pass(); close(done) }()
	for i := range pollerWorkers {
		select {
		case <-g.arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d polls started concurrently, want %d", i, pollerWorkers)
		}
	}
	select {
	case id := <-g.arrived:
		t.Fatalf("poll of %s started while %d were in flight; the bound is %d", id, pollerWorkers, pollerWorkers)
	case <-time.After(50 * time.Millisecond):
	}
	g.releaseAll()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pass did not finish after release")
	}
	if got := len(g.arrived) + pollerWorkers; got != n {
		t.Fatalf("%d instances polled, want %d", got, n)
	}
	if int(g.peak.Load()) != pollerWorkers {
		t.Fatalf("peak concurrency %d, want %d", g.peak.Load(), pollerWorkers)
	}
}

// runtimeSnapshot reads every instance's runtime off the hub loop.
func runtimeSnapshot(h *hub) map[string]*fleetgrpc.InstanceRuntime {
	ch := make(chan map[string]*fleetgrpc.InstanceRuntime, 1)
	h.post(func(h *hub) {
		out := make(map[string]*fleetgrpc.InstanceRuntime, len(h.runtime))
		for k, r := range h.runtime {
			out[k] = cloneRuntime(r)
		}
		ch <- out
	})
	return <-ch
}

func TestSessionsPassListsInstancesOnBoundedWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub()
	go h.run(ctx)
	const n = pollerWorkers + 4
	g := newGatedBackend(t, n)
	seedRunning(t, h, n, g)

	assertBoundedPass(t, g, n, func() { sessionsPass(h) })

	// Each instance's own sessions land on its own runtime entry.
	rt := runtimeSnapshot(h)
	for i := range n {
		key := runtimeKey("alpha", fmt.Sprintf("i%02d", i))
		s := rt[key].GetSessions()
		if want := fmt.Sprintf("main-c%02d", i); len(s) != 1 || s[0].GetName() != want {
			t.Fatalf("%s sessions = %v, want [%s]", key, s, want)
		}
	}
}

func TestStatsActivityPassCapturesOnBoundedWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub()
	go h.run(ctx)
	const n = pollerWorkers + 4
	g := newGatedBackend(t, n)
	seedRunning(t, h, n, g)

	assertBoundedPass(t, g, n, func() { statsActivityPass(h) })

	// Every running instance's backend stays cached (none pruned as inactive),
	// and every instance got a runtime update from the pass.
	h.backendsMu.Lock()
	cached := len(h.backends)
	h.backendsMu.Unlock()
	if cached != n {
		t.Fatalf("%d backends cached after the pass, want %d", cached, n)
	}
	if rt := runtimeSnapshot(h); len(rt) != n {
		t.Fatalf("%d instances got runtime, want %d", len(rt), n)
	}
}

// TestForEachBoundedPollsShareBackendsSafely runs the two bounded passes at
// once over the same cached backends, as the sessions and stats pollers do, so
// `go test -race` sees any shared write the pool introduced.
func TestForEachBoundedPollsShareBackendsSafely(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub()
	go h.run(ctx)
	const n = 2 * pollerWorkers
	g := newGatedBackend(t, 2*n)
	g.releaseAll()
	seedRunning(t, h, n, g)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); sessionsPass(h) }()
	go func() { defer wg.Done(); statsActivityPass(h) }()
	wg.Wait()
	if got := len(g.arrived); got != 2*n {
		t.Fatalf("%d polls, want %d", got, 2*n)
	}
}

// failingBackend fails every capture and reports stats (the instance answers
// docker stats, say, but not its execs).
type failingBackend struct{ backend.Backend }

func (failingBackend) CaptureAllSessions(string) backend.AllSessions { return backend.AllSessions{} }
func (failingBackend) AgentToolProbe(string) (string, bool)          { return "", false }
func (failingBackend) Stats(ids []string) (map[string]*backend.ContainerStats, error) {
	out := map[string]*backend.ContainerStats{}
	for _, id := range ids {
		out[id] = &backend.ContainerStats{CPUMillicores: 5, MemoryMB: 10}
	}
	return out, nil
}

// An instance whose captures keep failing turns unknown after unknownAfter
// passes (about 15s), and its stats go blank with it.
func TestStatsActivityPassBlanksAnUnknownInstance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub()
	go h.run(ctx)
	seedRunning(t, h, 1, failingBackend{})
	key := runtimeKey("alpha", "i00")
	for i := 1; i <= unknownAfter; i++ {
		statsActivityPass(h)
		rt := runtimeSnapshot(h)[key]
		unknown := rt.GetAgentActivity() == fleetgrpc.AgentActivity_AGENT_ACTIVITY_UNKNOWN
		if i < unknownAfter {
			if unknown || rt.GetStats() == nil {
				t.Fatalf("pass %d: activity %v stats %v, want the stats and no unknown yet", i, rt.GetAgentActivity(), rt.GetStats())
			}
			continue
		}
		if !unknown || rt.GetStats() != nil {
			t.Fatalf("pass %d: activity %v stats %v, want unknown and no stats", i, rt.GetAgentActivity(), rt.GetStats())
		}
	}
}
