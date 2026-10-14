package fleetclient

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// TestDialSocketReachesTheNamedSocket: DialSocket reaches the daemon serving
// the socket it names (not the default one), with one Hello and no reconcile.
func TestDialSocketReachesTheNamedSocket(t *testing.T) {
	home := probeHome(t)
	sock := filepath.Join(home, "other.sock")
	lis, err := net.Listen("unix", sock)
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
	conn, err := DialSocket(ctx, sock)
	if err != nil {
		t.Fatalf("DialSocket: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Service().GetArmada(ctx, &fleetgrpc.GetArmadaRequest{}); err != nil {
		t.Fatalf("an RPC over the dialled socket: %v", err)
	}
	if n := fake.hellos.Load(); n != 1 {
		t.Fatalf("%d Hello handshakes, want 1", n)
	}
}

// TestDialSocketNeverSpawnsADaemon: with nothing listening it fails fast and
// leaves no trace of a spawn (DialLocal would start a daemon).
func TestDialSocketNeverSpawnsADaemon(t *testing.T) {
	home := probeHome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if conn, err := DialSocket(ctx, ""); err == nil {
		_ = conn.Close()
		t.Fatal("DialSocket with no daemon succeeded")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("DialSocket took %s: it should fail fast, not wait for a daemon", elapsed)
	}
	if _, err := os.Stat(filepath.Join(home, ".fleet")); !os.IsNotExist(err) {
		t.Fatalf("DialSocket touched ~/.fleet (a spawn attempt): stat err = %v", err)
	}
}
