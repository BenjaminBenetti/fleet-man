package server

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// TestWatchStreamsInitialState exercises the real Watch RPC over an in-process
// bufconn: a client that subscribes with include_initial_state must receive a
// StateChanged carrying the hub's current snapshot. This validates the streaming
// plumbing (registration + initial-snapshot-in-one-turn + stream.Send + the
// Event oneof) that the TUI read-path depends on.
func TestWatchStreamsInitialState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc := newService()
	go svc.hub.run(ctx)

	// Seed the hub's snapshot (bypassing the disk poller).
	seeded := &fleetgrpc.State{
		Fleets: map[string]*fleetgrpc.Fleet{
			"alpha": {Name: "alpha", Instances: []*fleetgrpc.Instance{{Name: "agent-1"}}},
		},
	}
	synced := make(chan struct{})
	svc.hub.post(func(h *hub) { h.setState(seeded); close(synced) })
	<-synced

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gs, svc)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Second)
	defer streamCancel()
	stream, err := fleetgrpc.NewFleetServiceClient(conn).Watch(streamCtx, &fleetgrpc.WatchRequest{IncludeInitialState: true})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	ev, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	sc := ev.GetStateChanged()
	if sc == nil {
		t.Fatalf("first event is not StateChanged: %T", ev.GetKind())
	}
	if !proto.Equal(sc.GetState(), seeded) {
		t.Fatalf("initial snapshot mismatch:\n got %v\nwant %v", sc.GetState(), seeded)
	}
}

// TestWatchPushesOnStateChange verifies a subscriber receives a follow-up
// StateChanged when the hub's snapshot changes after subscription.
func TestWatchPushesOnStateChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc := newService()
	go svc.hub.run(ctx)

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gs, svc)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Second)
	defer streamCancel()
	// No initial state; we only want the post-subscription push.
	stream, err := fleetgrpc.NewFleetServiceClient(conn).Watch(streamCtx, &fleetgrpc.WatchRequest{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Give the server a moment to register the subscriber, then change state.
	time.Sleep(100 * time.Millisecond)
	svc.hub.post(func(h *hub) {
		h.setState(&fleetgrpc.State{LastSeenVersion: proto.String("v9")})
	})

	ev, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if got := ev.GetStateChanged().GetState().GetLastSeenVersion(); got != "v9" {
		t.Fatalf("want pushed state v9, got %q", got)
	}
}

// TestWatchStreamsRemoteMcpStatus validates the computed-field-over-Watch design:
// the gateway-assigned Public MCP URL is delivered as a RemoteMcpStatus event
// (never a Config field), and a subscriber with include_initial_state receives
// the hub's cached status up front.
func TestWatchStreamsRemoteMcpStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc := newService()
	go svc.hub.run(ctx)

	// Seed the hub's cached tunnel status (what the tunnel manager will push in
	// a later PR).
	want := &fleetgrpc.RemoteMcpStatus{
		State:     fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED,
		PublicUrl: "https://gw.example.com/mcp/deadbeef",
	}
	synced := make(chan struct{})
	svc.hub.post(func(h *hub) { h.broadcastRemoteMcpStatus(want); close(synced) })
	<-synced

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gs, svc)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Second)
	defer streamCancel()
	stream, err := fleetgrpc.NewFleetServiceClient(conn).Watch(streamCtx, &fleetgrpc.WatchRequest{IncludeInitialState: true})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// The initial snapshot may interleave StateChanged with the status; read
	// until the RemoteMcpStatus event arrives.
	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv before RemoteMcpStatus: %v", err)
		}
		if rm := ev.GetRemoteMcpStatus(); rm != nil {
			if rm.GetState() != fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED || rm.GetPublicUrl() != want.GetPublicUrl() {
				t.Fatalf("remote-mcp status mismatch: %v", rm)
			}
			return
		}
	}
}

// TestWatchStreamsFileCopy verifies the fc plumbing's middle hop: a file.copy
// broadcast (what the control registry posts when an in-container `fleet copy`
// sends its envelope) is delivered to a Watch subscriber as a FileCopy event,
// in order, tagged with the originating instance.
func TestWatchStreamsFileCopy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc := newService()
	go svc.hub.run(ctx)

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gs, svc)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Second)
	defer streamCancel()
	stream, err := fleetgrpc.NewFleetServiceClient(conn).Watch(streamCtx, &fleetgrpc.WatchRequest{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Give the server a moment to register the subscriber, then broadcast two
	// copies — discrete events must arrive in order, not conflated.
	time.Sleep(100 * time.Millisecond)
	svc.hub.post(func(h *hub) {
		h.broadcastFileCopy(&fleetgrpc.FileCopy{Fleet: "alpha", Instance: "i1", Path: "/ws/bin/tool"})
		h.broadcastFileCopy(&fleetgrpc.FileCopy{Fleet: "alpha", Instance: "i1", Path: "/ws/bin/tool2"})
	})

	for _, wantPath := range []string{"/ws/bin/tool", "/ws/bin/tool2"} {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		fc := ev.GetFileCopy()
		if fc == nil {
			t.Fatalf("want FileCopy event, got %v", ev)
		}
		if fc.GetFleet() != "alpha" || fc.GetInstance() != "i1" || fc.GetPath() != wantPath {
			t.Fatalf("FileCopy = %v, want alpha/i1 %s", fc, wantPath)
		}
	}
}
