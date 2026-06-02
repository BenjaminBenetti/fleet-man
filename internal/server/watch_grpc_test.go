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
