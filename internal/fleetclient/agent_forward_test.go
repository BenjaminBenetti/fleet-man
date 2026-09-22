package fleetclient

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
)

// probeHome points HOME at a fresh short directory (a unix socket path under
// t.TempDir() can exceed the platform's 104-byte sun_path limit).
func probeHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fmprobe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("HOME", dir)
	return dir
}

// TestProbeLocalArmadaNeverSpawnsADaemon: with no daemon listening the probe
// fails fast and leaves no trace of a spawn. DialLocal would have created
// ~/.fleet for its spawn lock before trying to start one.
func TestProbeLocalArmadaNeverSpawnsADaemon(t *testing.T) {
	home := probeHome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	if remotes, err := ProbeLocalArmada(ctx); err == nil {
		t.Fatalf("probe with no daemon = %v, want an error", remotes)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("probe took %s: it should fail fast, not wait for a daemon", elapsed)
	}
	if _, err := os.Stat(filepath.Join(home, ".fleet")); !os.IsNotExist(err) {
		t.Fatalf("the probe touched ~/.fleet (a spawn attempt): stat err = %v", err)
	}
}

// armadaOnlyServer answers GetArmada and counts Hello handshakes.
type armadaOnlyServer struct {
	fleetgrpc.UnimplementedFleetServiceServer
	hellos atomic.Int32
}

func (s *armadaOnlyServer) Hello(context.Context, *fleetgrpc.HelloRequest) (*fleetgrpc.HelloReply, error) {
	s.hellos.Add(1)
	return &fleetgrpc.HelloReply{}, nil
}

func (s *armadaOnlyServer) GetArmada(context.Context, *fleetgrpc.GetArmadaRequest) (*fleetgrpc.GetArmadaReply, error) {
	return &fleetgrpc.GetArmadaReply{Remotes: []*fleetgrpc.ArmadaRemote{
		{Url: "ssh://ben@devbox", ForwardAgent: true},
		{Url: "https://gw.example.com/abc", Token: "t"},
	}}, nil
}

// TestProbeLocalArmadaReadsARunningDaemon: a listening daemon's registry comes
// back as is, without the version handshake (and so without any reconcile).
func TestProbeLocalArmadaReadsARunningDaemon(t *testing.T) {
	home := probeHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".fleet"), 0o700); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", filepath.Join(home, ".fleet", "fleet.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	fake := &armadaOnlyServer{}
	fleetgrpc.RegisterFleetServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	remotes, err := ProbeLocalArmada(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(remotes) != 2 || remotes[0].GetUrl() != "ssh://ben@devbox" || !remotes[0].GetForwardAgent() || remotes[1].GetForwardAgent() {
		t.Fatalf("remotes = %v", remotes)
	}
	if n := fake.hellos.Load(); n != 0 {
		t.Fatalf("the probe ran %d Hello handshakes; it must never reconcile the daemon", n)
	}
}
