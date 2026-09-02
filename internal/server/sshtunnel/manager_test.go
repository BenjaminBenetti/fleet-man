package sshtunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// remoteDaemon is a stand-in for the remote's token-gated gRPC server: it
// answers Hello only with the right bearer token, on a loopback port.
type remoteDaemon struct {
	fleetgrpc.UnimplementedFleetServiceServer
	token string
	srv   *grpc.Server
	port  int
}

func (r *remoteDaemon) Hello(ctx context.Context, _ *fleetgrpc.HelloRequest) (*fleetgrpc.HelloReply, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("authorization"); len(v) == 0 || v[0] != "Bearer "+r.token {
		return nil, status.Error(codes.Unauthenticated, "invalid token")
	}
	return &fleetgrpc.HelloReply{ServerVersion: "remote"}, nil
}

func startRemoteDaemon(t *testing.T, token string) *remoteDaemon {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &remoteDaemon{token: token, srv: grpc.NewServer(), port: lis.Addr().(*net.TCPAddr).Port}
	fleetgrpc.RegisterFleetServiceServer(r.srv, r)
	go func() { _ = r.srv.Serve(lis) }()
	t.Cleanup(r.srv.Stop)
	return r
}

// fakeForward is an in-process "ssh -L": a loopback listener on localPort that
// pipes each connection to remotePort. Kill closes it (Done fires).
type fakeForward struct {
	lis  net.Listener
	done chan struct{}
	once sync.Once
	err  string
}

func (f *fakeForward) Done() <-chan struct{} { return f.done }
func (f *fakeForward) Err() string           { return f.err }
func (f *fakeForward) Kill() {
	f.once.Do(func() {
		if f.lis != nil {
			_ = f.lis.Close()
		}
		close(f.done)
	})
}

func newFakeForward(localPort, remotePort int) (forwardProc, error) {
	lis, err := net.Listen("tcp", loopback(localPort))
	if err != nil {
		f := &fakeForward{done: make(chan struct{}), err: "bind [127.0.0.1]:" + loopback(localPort) + ": Address already in use"}
		f.Kill() // like ssh with ExitOnForwardFailure: exits at once with the bind diagnostic
		return f, nil
	}
	f := &fakeForward{lis: lis, done: make(chan struct{})}
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r, err := net.Dial("tcp", loopback(remotePort))
				if err != nil {
					return // ssh closes the channel when the remote connect fails
				}
				defer r.Close()
				// Like ssh's channel: when EITHER side hangs up, tear the pair
				// down — a readiness probe that connects and closes must release
				// the remote's accepted socket too, or a grpc server sits in its
				// preface handshake (and its Stop waits on it).
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(r, c); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, r); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return f, nil
}

// newTestManager wires a Manager whose discovery is a stub and whose forward is
// in-process, keeping the REAL hello check (so the Hello round trip through the
// forward is what proves the tunnel).
func newTestManager(t *testing.T, disc func() (Discovery, error)) (*Manager, *int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := New(ctx)
	spawns := 0
	m.discover = func(context.Context, Target) (Discovery, error) { return disc() }
	m.forward = func(_ context.Context, _ Target, localPort, remotePort int) (forwardProc, error) {
		spawns++
		return newFakeForward(localPort, remotePort)
	}
	t.Cleanup(m.Close)
	return m, &spawns
}

func TestResolveBringsUpAndReusesTunnel(t *testing.T) {
	rd := startRemoteDaemon(t, "tok")
	m, spawns := newTestManager(t, func() (Discovery, error) { return Discovery{Port: rd.port, Token: "tok"}, nil })

	ep, err := m.Resolve(context.Background(), "ssh://ben@desktop")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ep.Token != "tok" || !strings.HasPrefix(ep.Addr, "127.0.0.1:") {
		t.Fatalf("endpoint = %+v", ep)
	}
	// A second Resolve (another spelling of the same remote) reuses the live
	// forward: same address, no new spawn.
	ep2, err := m.Resolve(context.Background(), "SSH://ben@Desktop/")
	if err != nil {
		t.Fatalf("Resolve again: %v", err)
	}
	if ep2 != ep || *spawns != 1 {
		t.Fatalf("second resolve: %+v (spawns=%d), want reuse of %+v", ep2, *spawns, ep)
	}
}

// TestResolveRebuildsStaleTunnel simulates the remote daemon restarting on a
// NEW port: the live forward now points at a dead port, the Hello check fails,
// and Resolve re-discovers and rebuilds — on the same local port.
func TestResolveRebuildsStaleTunnel(t *testing.T) {
	rd := startRemoteDaemon(t, "tok")
	current := rd
	m, spawns := newTestManager(t, func() (Discovery, error) { return Discovery{Port: current.port, Token: current.token}, nil })

	ep, err := m.Resolve(context.Background(), "ssh://desktop")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	rd.srv.Stop()                          // remote daemon goes away…
	current = startRemoteDaemon(t, "tok2") // …and comes back elsewhere with a fresh token

	ep2, err := m.Resolve(context.Background(), "ssh://desktop")
	if err != nil {
		t.Fatalf("Resolve after restart: %v", err)
	}
	if ep2.Addr != ep.Addr {
		t.Fatalf("local address should stay stable across a rebuild: %s -> %s", ep.Addr, ep2.Addr)
	}
	if ep2.Token != "tok2" || *spawns != 2 {
		t.Fatalf("rebuild: %+v spawns=%d, want new token + one respawn", ep2, *spawns)
	}
}

// TestResolveMovesOffTakenPort: the tunnel's remembered local port is now held
// by another process — Resolve must notice before spawning and move to a fresh
// port (one spawn, never pointed at the squatter).
func TestResolveMovesOffTakenPort(t *testing.T) {
	rd := startRemoteDaemon(t, "tok")
	m, spawns := newTestManager(t, func() (Discovery, error) { return Discovery{Port: rd.port, Token: "tok"}, nil })

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	tgt, _ := ParseURL("ssh://desktop")
	m.tunnelFor(tgt).localPort = taken.Addr().(*net.TCPAddr).Port

	ep, err := m.Resolve(context.Background(), "ssh://desktop")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ep.Addr == taken.Addr().String() || *spawns != 1 {
		t.Fatalf("expected a fresh port without a wasted spawn: %+v spawns=%d", ep, *spawns)
	}
}

// TestResolveRetriesBindRace: the port is free at the pre-check but ssh still
// fails to bind it (a squatter in ssh's connect window). The forward exits with
// the bind diagnostic; Resolve must retry on another port.
func TestResolveRetriesBindRace(t *testing.T) {
	rd := startRemoteDaemon(t, "tok")
	m, spawns := newTestManager(t, func() (Discovery, error) { return Discovery{Port: rd.port, Token: "tok"}, nil })
	var squatter net.Listener
	realForward := m.forward
	m.forward = func(ctx context.Context, t Target, localPort, remotePort int) (forwardProc, error) {
		if *spawns == 0 {
			// Grab the port just before "ssh" binds it.
			l, err := net.Listen("tcp", loopback(localPort))
			if err != nil {
				return nil, err
			}
			squatter = l
		}
		return realForward(ctx, t, localPort, remotePort)
	}
	defer func() {
		if squatter != nil {
			_ = squatter.Close()
		}
	}()

	ep, err := m.Resolve(context.Background(), "ssh://desktop")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ep.Addr == squatter.Addr().String() || *spawns != 2 {
		t.Fatalf("expected a retry on a fresh port: %+v spawns=%d", ep, *spawns)
	}
}

func TestResolveSurfacesDiscoveryErrors(t *testing.T) {
	m, spawns := newTestManager(t, func() (Discovery, error) { return Discovery{}, ErrSSHModeOff })
	if _, err := m.Resolve(context.Background(), "ssh://desktop"); !errors.Is(err, ErrSSHModeOff) {
		t.Fatalf("want ErrSSHModeOff, got %v", err)
	}
	if *spawns != 0 {
		t.Fatal("no forward should be spawned when discovery fails")
	}
	if _, err := m.Resolve(context.Background(), "https://gw/abc"); err == nil {
		t.Fatal("a non-ssh URL must be rejected")
	}
}

// TestResolveWrongTokenFails: the forward comes up but the remote refuses the
// discovered token — Resolve reports it and tears the forward down.
func TestResolveWrongTokenFails(t *testing.T) {
	rd := startRemoteDaemon(t, "real")
	m, _ := newTestManager(t, func() (Discovery, error) { return Discovery{Port: rd.port, Token: "stale"}, nil })
	_, err := m.Resolve(context.Background(), "ssh://desktop")
	if err == nil || status.Code(errors.Unwrap(err)) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
	tgt, _ := ParseURL("ssh://desktop")
	if m.tunnelFor(tgt).proc != nil {
		t.Fatal("failed verification must not leave a forward running")
	}
}

func TestPruneKillsDroppedRemotes(t *testing.T) {
	rd := startRemoteDaemon(t, "tok")
	m, _ := newTestManager(t, func() (Discovery, error) { return Discovery{Port: rd.port, Token: "tok"}, nil })
	if _, err := m.Resolve(context.Background(), "ssh://a"); err != nil {
		t.Fatal(err)
	}
	epB, err := m.Resolve(context.Background(), "ssh://b")
	if err != nil {
		t.Fatal(err)
	}
	m.Prune([]string{"ssh://b", "https://gw/keep-me-ignored"})
	if len(m.tunnels) != 1 {
		t.Fatalf("tunnels after prune = %d, want 1", len(m.tunnels))
	}
	// b still serves; a's forward is gone.
	if err := helloThrough(context.Background(), epB.Addr, "tok"); err != nil {
		t.Fatalf("kept remote should still answer: %v", err)
	}
	m.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := net.DialTimeout("tcp", epB.Addr, 100*time.Millisecond); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Close should tear down every forward")
}
