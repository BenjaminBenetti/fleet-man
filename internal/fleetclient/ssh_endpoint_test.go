package fleetclient

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestIsSSHURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"ssh://ben@desktop":             true,
		"  SSH://desktop:2222 ":         true,
		"https://gw.example.com/abc":    false,
		"http://gw.example.com:50051/x": false,
		"desktop":                       false,
		"":                              false,
	} {
		if got := IsSSHURL(raw); got != want {
			t.Errorf("IsSSHURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

// stubResolve installs a resolveSSHRemote stub returning the given sequence of
// results (the last repeats) and returns a call counter.
func stubResolve(t *testing.T, results ...[2]string) *atomic.Int32 {
	t.Helper()
	orig := resolveSSHRemote
	var calls atomic.Int32
	resolveSSHRemote = func(_ context.Context, rawURL string) (string, string, error) {
		n := int(calls.Add(1)) - 1
		if n >= len(results) {
			n = len(results) - 1
		}
		return results[n][0], results[n][1], nil
	}
	t.Cleanup(func() { resolveSSHRemote = orig })
	return &calls
}

// TestSelectEndpointSSH: FLEET_SSH resolves through the local daemon into an
// endpoint whose per-RPC credentials carry the discovered token; FLEET_GATEWAY
// still wins when both are set (the TUI only ever sets one); the endpoint is
// remote.
func TestSelectEndpointSSH(t *testing.T) {
	t.Setenv(EnvGateway, "")
	t.Setenv(EnvServer, "")
	t.Setenv(EnvSSH, "ssh://ben@desktop")
	calls := stubResolve(t, [2]string{"127.0.0.1:40001", "tok-remote"})

	ep, err := selectEndpoint(context.Background())
	if err != nil {
		t.Fatalf("selectEndpoint: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("resolve calls = %d, want 1", calls.Load())
	}
	if ep.Target() != sshTarget || ep.IsLocal() || ep.String() != "ssh://ben@desktop" {
		t.Fatalf("endpoint = %s / target %s / local %v", ep, ep.Target(), ep.IsLocal())
	}
	sshEp, ok := ep.(sshEndpoint)
	if !ok {
		t.Fatalf("endpoint type %T", ep)
	}
	md, err := sshPerRPC{state: sshEp.state}.GetRequestMetadata(context.Background())
	if err != nil || md["authorization"] != "Bearer tok-remote" {
		t.Fatalf("per-RPC metadata = %v, %v", md, err)
	}
	if !IsRemote() || !IsSSH() || IsGateway() {
		t.Fatalf("IsRemote=%v IsSSH=%v IsGateway=%v", IsRemote(), IsSSH(), IsGateway())
	}

	t.Setenv(EnvGateway, "https://gw.example.com/abc")
	if ep, err := selectEndpoint(context.Background()); err != nil || ep.String() != "https://gw.example.com/abc" {
		t.Fatalf("FLEET_GATEWAY should take precedence: %v, %v", ep, err)
	}
}

func TestSelectEndpointSSHErrors(t *testing.T) {
	t.Setenv(EnvGateway, "")
	t.Setenv(EnvServer, "")

	t.Setenv(EnvSSH, "desktop")
	if _, err := selectEndpoint(context.Background()); err == nil || !strings.Contains(err.Error(), "ssh://") {
		t.Fatalf("malformed FLEET_SSH: %v", err)
	}

	t.Setenv(EnvSSH, "ssh://desktop")
	orig := resolveSSHRemote
	resolveSSHRemote = func(context.Context, string) (string, string, error) {
		return "", "", errors.New("ssh: Permission denied (publickey).")
	}
	defer func() { resolveSSHRemote = orig }()
	if _, err := selectEndpoint(context.Background()); err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("resolve failure should surface verbatim: %v", err)
	}
}

// listenTCP returns a loopback listener that accepts (and holds) connections.
func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { time.Sleep(time.Second); _ = c.Close() }()
		}
	}()
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// TestSSHDialReResolvesOnReconnect: the first connect uses the address the
// endpoint was resolved with (no second resolve); every later connect
// re-resolves through the local daemon and refreshes the token — which is what
// moves a live client onto the tunnel's new port after a local daemon restart.
func TestSSHDialReResolvesOnReconnect(t *testing.T) {
	a, b := listenTCP(t), listenTCP(t)
	calls := stubResolve(t, [2]string{a.Addr().String(), "tok-a"}, [2]string{b.Addr().String(), "tok-b"})

	ep, err := newSSHEndpoint(context.Background(), "ssh://desktop")
	if err != nil {
		t.Fatal(err)
	}
	c1, err := ep.dial(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer c1.Close()
	if c1.RemoteAddr().String() != a.Addr().String() || calls.Load() != 1 {
		t.Fatalf("first dial went to %s after %d resolves; want %s after 1", c1.RemoteAddr(), calls.Load(), a.Addr())
	}
	c2, err := ep.dial(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	defer c2.Close()
	if c2.RemoteAddr().String() != b.Addr().String() || calls.Load() != 2 {
		t.Fatalf("reconnect went to %s after %d resolves; want the re-resolved %s after 2", c2.RemoteAddr(), calls.Load(), b.Addr())
	}
	if md, _ := (sshPerRPC{state: ep.state}).GetRequestMetadata(context.Background()); md["authorization"] != "Bearer tok-b" {
		t.Fatalf("token not refreshed by the re-resolve: %v", md)
	}
}

// helloServer answers Hello on a loopback port, echoing the bearer token it saw
// as the server version.
type helloServer struct {
	fleetgrpc.UnimplementedFleetServiceServer
}

func (helloServer) Hello(ctx context.Context, _ *fleetgrpc.HelloRequest) (*fleetgrpc.HelloReply, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	v := md.Get("authorization")
	if len(v) == 0 {
		return nil, errors.New("no token")
	}
	return &fleetgrpc.HelloReply{ServerVersion: strings.TrimPrefix(v[0], "Bearer ")}, nil
}

// TestSSHClientConnHealsAfterTunnelMoves is the QA scenario end to end at the
// gRPC level: a ClientConn built from an ssh endpoint whose original tunnel
// port is dead gets, on gRPC's reconnect, the tunnel's NEW port from a fresh
// resolve — and its RPCs carry the token from that resolve.
func TestSSHClientConnHealsAfterTunnelMoves(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	_ = dead.Close() // the old forward: gone with the old local daemon

	live, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(srv, helloServer{})
	go func() { _ = srv.Serve(live) }()
	t.Cleanup(srv.Stop)

	stubResolve(t, [2]string{deadAddr, "tok-old"}, [2]string{live.Addr().String(), "tok-new"})
	ep, err := newSSHEndpoint(context.Background(), "ssh://desktop")
	if err != nil {
		t.Fatal(err)
	}
	cc, err := grpc.NewClient(ep.Target(), ep.DialOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// WaitForReady rides out the first (refused) connect and gRPC's backoff
	// before the re-resolving reconnect lands on the live port.
	reply, err := fleetgrpc.NewFleetServiceClient(cc).Hello(ctx, &fleetgrpc.HelloRequest{}, grpc.WaitForReady(true))
	if err != nil {
		t.Fatalf("Hello after the tunnel moved: %v", err)
	}
	if reply.GetServerVersion() != "tok-new" {
		t.Fatalf("RPC carried token %q, want the re-resolved tok-new", reply.GetServerVersion())
	}
}
