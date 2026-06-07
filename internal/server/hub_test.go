package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/protobuf/proto"
)

// drainSync posts a no-op closure and waits for it, guaranteeing every
// previously-posted closure has been processed by the hub loop (FIFO, single
// consumer).
func drainSync(t *testing.T, h *hub) {
	t.Helper()
	synced := make(chan struct{})
	if !h.post(func(*hub) { close(synced) }) {
		t.Fatal("hub.post failed (loop stopped)")
	}
	<-synced
}

func TestSubscriberStateConflation(t *testing.T) {
	s := newSubscriber(false)
	s.enqueueState(&fleetgrpc.State{LastSeenVersion: proto.String("v1")})
	s.enqueueState(&fleetgrpc.State{LastSeenVersion: proto.String("v2")})

	st, _, _, _ := s.drain()
	if st.GetLastSeenVersion() != "v2" {
		t.Fatalf("conflation: want newest v2, got %q", st.GetLastSeenVersion())
	}
	if again, _, _, _ := s.drain(); again != nil {
		t.Fatalf("want nil state after drain, got %v", again)
	}
}

func TestSubscriberRuntimeConflationByKey(t *testing.T) {
	s := newSubscriber(true)
	// Two complete pushes for the same instance: newest wins.
	s.enqueueRuntime([]*fleetgrpc.InstanceRuntime{{Fleet: "f", Instance: "i", LiveStatus: fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_RUNNING}})
	s.enqueueRuntime([]*fleetgrpc.InstanceRuntime{{Fleet: "f", Instance: "i", LiveStatus: fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_STOPPED}})
	// A different instance survives independently.
	s.enqueueRuntime([]*fleetgrpc.InstanceRuntime{{Fleet: "f", Instance: "j", LiveStatus: fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_RUNNING}})

	_, rt, _, _ := s.drain()
	if len(rt) != 2 {
		t.Fatalf("want 2 keyed entries, got %d", len(rt))
	}
	byKey := map[string]*fleetgrpc.InstanceRuntime{}
	for _, r := range rt {
		byKey[runtimeKey(r.GetFleet(), r.GetInstance())] = r
	}
	if got := byKey["f/i"].GetLiveStatus(); got != fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_STOPPED {
		t.Fatalf("f/i: want newest STOPPED, got %v", got)
	}
	if got := byKey["f/j"].GetLiveStatus(); got != fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_RUNNING {
		t.Fatalf("f/j: want RUNNING, got %v", got)
	}
}

func TestHubSetStateBroadcastsAndDedups(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub()
	go h.run(ctx)

	sub := newSubscriber(false)
	if !h.post(func(h *hub) { h.addSub(sub) }) {
		t.Fatal("addSub post failed")
	}
	h.post(func(h *hub) { h.setState(&fleetgrpc.State{LastSeenVersion: proto.String("v1")}) })
	// An identical state must NOT re-broadcast (proto.Equal dedup).
	h.post(func(h *hub) { h.setState(&fleetgrpc.State{LastSeenVersion: proto.String("v1")}) })
	drainSync(t, h)

	st, _, _, _ := sub.drain()
	if st.GetLastSeenVersion() != "v1" {
		t.Fatalf("want broadcast v1, got %q", st.GetLastSeenVersion())
	}
	// After draining the single broadcast, the dedup'd second setState left
	// nothing pending.
	if again, _, _, _ := sub.drain(); again != nil {
		t.Fatalf("dedup: want no second broadcast, got %v", again)
	}
}

func TestHubBackpressureDoesNotBlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub()
	go h.run(ctx)

	slow := newSubscriber(false) // never drains
	fast := newSubscriber(false)
	if !h.post(func(h *hub) { h.addSub(slow); h.addSub(fast) }) {
		t.Fatal("addSub post failed")
	}

	// Many distinct states; a never-draining subscriber must not stall the loop.
	for i := 0; i < 200; i++ {
		v := fmt.Sprintf("v%d", i)
		if !h.post(func(h *hub) { h.setState(&fleetgrpc.State{LastSeenVersion: &v}) }) {
			t.Fatalf("post %d failed (loop blocked?)", i)
		}
	}
	drainSync(t, h)

	st, _, _, _ := fast.drain()
	if st == nil {
		t.Fatal("fast subscriber received nothing")
	}
	if st.GetLastSeenVersion() != "v199" {
		t.Fatalf("fast subscriber: want newest v199, got %q", st.GetLastSeenVersion())
	}
}

func TestSubscriberRemoteMcpConflation(t *testing.T) {
	s := newSubscriber(false)
	s.enqueueRemoteMcp(&fleetgrpc.RemoteMcpStatus{State: fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTING})
	s.enqueueRemoteMcp(&fleetgrpc.RemoteMcpStatus{
		State:     fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED,
		PublicUrl: "https://gw/mcp/abc",
	})

	_, _, _, rm := s.drain()
	if rm.GetState() != fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED || rm.GetPublicUrl() != "https://gw/mcp/abc" {
		t.Fatalf("conflation: want newest CONNECTED+url, got %v", rm)
	}
	if _, _, _, again := s.drain(); again != nil {
		t.Fatalf("want nil remote-mcp status after drain, got %v", again)
	}
}

func TestHubRemoteMcpStatusBroadcastsAndDedups(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub()
	go h.run(ctx)

	sub := newSubscriber(false)
	if !h.post(func(h *hub) { h.addSub(sub) }) {
		t.Fatal("addSub post failed")
	}
	status := &fleetgrpc.RemoteMcpStatus{
		State:     fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED,
		PublicUrl: "https://gw/mcp/xyz",
	}
	h.post(func(h *hub) { h.broadcastRemoteMcpStatus(status) })
	// An identical status must NOT re-broadcast (proto.Equal dedup).
	h.post(func(h *hub) {
		h.broadcastRemoteMcpStatus(&fleetgrpc.RemoteMcpStatus{
			State:     fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED,
			PublicUrl: "https://gw/mcp/xyz",
		})
	})
	drainSync(t, h)

	_, _, _, rm := sub.drain()
	if rm.GetPublicUrl() != "https://gw/mcp/xyz" {
		t.Fatalf("want broadcast status, got %v", rm)
	}
	if _, _, _, again := sub.drain(); again != nil {
		t.Fatalf("dedup: want no second broadcast, got %v", again)
	}

	// The hub caches the latest status for the initial snapshot of a future
	// subscriber.
	var cached *fleetgrpc.RemoteMcpStatus
	synced := make(chan struct{})
	h.post(func(h *hub) { cached = h.remoteMcp; close(synced) })
	<-synced
	if cached.GetPublicUrl() != "https://gw/mcp/xyz" {
		t.Fatalf("hub did not cache latest status: %v", cached)
	}
}

func TestHubRuntimeGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub()
	go h.run(ctx)

	if h.runtimeWanted.Load() {
		t.Fatal("gate should start false")
	}
	plain := newSubscriber(false)
	runtimeSub := newSubscriber(true)
	h.post(func(h *hub) { h.addSub(plain) })
	drainSync(t, h)
	if h.runtimeWanted.Load() {
		t.Fatal("gate should stay false with only a non-runtime subscriber")
	}
	h.post(func(h *hub) { h.addSub(runtimeSub) })
	drainSync(t, h)
	if !h.runtimeWanted.Load() {
		t.Fatal("gate should be true once a runtime subscriber joins")
	}
	// Edge should have fired.
	select {
	case <-h.runtimeEdge:
	default:
		t.Fatal("runtimeEdge should have fired on false->true")
	}
	h.post(func(h *hub) { h.removeSub(runtimeSub) })
	drainSync(t, h)
	if h.runtimeWanted.Load() {
		t.Fatal("gate should drop to false after the runtime subscriber leaves")
	}
}
