package server

import (
	"context"
	"net"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func dialBuf(t *testing.T, gs *grpc.Server) fleetgrpc.FleetServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return fleetgrpc.NewFleetServiceClient(conn)
}

// TestTunnelAuthInterceptors verifies the tunnel-facing gRPC server requires the
// MCP bearer token (unary + stream) and rejects missing/wrong tokens.
func TestTunnelAuthInterceptors(t *testing.T) {
	const token = "test-token-123"
	authUnary, authStream := bearerAuthInterceptors(token)
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(authUnary), grpc.ChainStreamInterceptor(authStream))
	fleetgrpc.RegisterFleetServiceServer(gs, newService())
	client := dialBuf(t, gs)

	withToken := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	if _, err := client.Hello(withToken, &fleetgrpc.HelloRequest{}); err != nil {
		t.Fatalf("Hello WITH token should succeed: %v", err)
	}
	if _, err := client.Hello(context.Background(), &fleetgrpc.HelloRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Hello WITHOUT token: want Unauthenticated, got %v", err)
	}
	wrong := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer nope")
	if _, err := client.Hello(wrong, &fleetgrpc.HelloRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Hello WRONG token: want Unauthenticated, got %v", err)
	}

	// Stream RPC is gated too: the interceptor rejects before the handler runs.
	stream, err := client.Watch(context.Background(), &fleetgrpc.WatchRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Watch WITHOUT token: want Unauthenticated, got %v", err)
	}
}

// TestLocalServerStaysAuthLess confirms a server built like the local unix-socket
// one (no interceptors) still serves without any token — the local path is
// unchanged.
func TestLocalServerStaysAuthLess(t *testing.T) {
	gs := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gs, newService())
	client := dialBuf(t, gs)
	if _, err := client.Hello(context.Background(), &fleetgrpc.HelloRequest{}); err != nil {
		t.Fatalf("local Hello without a token must succeed: %v", err)
	}
}

// TestTunnelDeniesArmadaEvenWithToken confirms the fleet-armada registry RPCs
// are refused over the tunnel-facing server even with a valid token: the
// registry holds OTHER fleets' bearer tokens and must never be reachable
// remotely. The same RPCs must still work on the auth-less local server.
func TestTunnelDeniesArmadaEvenWithToken(t *testing.T) {
	const token = "test-token-123"
	authUnary, authStream := bearerAuthInterceptors(token)
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(authUnary), grpc.ChainStreamInterceptor(authStream))
	fleetgrpc.RegisterFleetServiceServer(gs, newService())
	client := dialBuf(t, gs)

	withToken := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	if _, err := client.GetArmada(withToken, &fleetgrpc.GetArmadaRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("GetArmada over tunnel WITH token: want PermissionDenied, got %v", err)
	}
	if _, err := client.SetArmada(withToken, &fleetgrpc.SetArmadaRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("SetArmada over tunnel WITH token: want PermissionDenied, got %v", err)
	}
	if _, err := client.ResolveArmadaRemote(withToken, &fleetgrpc.ResolveArmadaRemoteRequest{Url: "ssh://x"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ResolveArmadaRemote over tunnel WITH token: want PermissionDenied, got %v", err)
	}

	// The same RPCs are served on the auth-less local server.
	isolateFleetDir(t)
	local := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(local, newService())
	localClient := dialBuf(t, local)
	if _, err := localClient.GetArmada(context.Background(), &fleetgrpc.GetArmadaRequest{}); err != nil {
		t.Fatalf("GetArmada on the local server must succeed: %v", err)
	}
}
