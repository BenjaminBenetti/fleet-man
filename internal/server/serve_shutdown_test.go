package server

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/mic"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// startServe runs the real serve loop against a throwaway HOME and returns a
// client on its unix socket plus the channel Serve's result lands on.
func startServe(t *testing.T) (fleetgrpc.FleetServiceClient, <-chan error) {
	t.Helper()
	// Not t.TempDir(): the socket lives under HOME and a unix socket path is
	// capped near 104 bytes, which a test-named temp dir can exceed.
	home, err := os.MkdirTemp("", "fleetd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	// The relay would point this test process's SSH_AUTH_SOCK at itself.
	t.Setenv(agentsock.EnvOverride, "off")

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx) }()
	t.Cleanup(cancel)

	conn, err := grpc.NewClient("unix://"+fleetpaths.SocketPath(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := fleetgrpc.NewFleetServiceClient(conn)

	hctx, hcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer hcancel()
	if _, err := client.Hello(hctx, &fleetgrpc.HelloRequest{}, grpc.WaitForReady(true)); err != nil {
		t.Fatalf("daemon never answered Hello: %v", err)
	}
	return client, served
}

// setDrainTimeout overrides the drain window for the rest of the test. Call it
// before startServe.
func setDrainTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := drainTimeout
	drainTimeout = d
	t.Cleanup(func() { drainTimeout = orig })
}

// openServedMicStream opens a Mic stream and sends its header.
func openServedMicStream(t *testing.T, client fleetgrpc.FleetServiceClient) fleetgrpc.FleetService_MicClient {
	t.Helper()
	stream, err := client.Mic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	open := &fleetgrpc.MicUp{Msg: &fleetgrpc.MicUp_Open{Open: &fleetgrpc.MicOpen{SampleRate: mic.SampleRate, Channels: mic.Channels}}}
	if err := stream.Send(open); err != nil {
		t.Fatal(err)
	}
	return stream
}

// shutdownAndWait sends the Shutdown RPC and fails unless Serve returns.
func shutdownAndWait(t *testing.T, client fleetgrpc.FleetServiceClient, served <-chan error) {
	t.Helper()
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Shutdown(sctx, &fleetgrpc.ShutdownRequest{Drain: true}); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("daemon still running 20s after Shutdown: an in-flight RPC is holding it")
	}
}

// TestServeShutdownEndsOpenStreams is the upgrade hang: a client holding a
// long-lived stream (here a TUI's Mic) must not keep a daemon that was asked
// to shut down alive. A daemon that lingers still holds the lifetime lock with
// its socket already gone, so its replacement can never start.
func TestServeShutdownEndsOpenStreams(t *testing.T) {
	// Far beyond the test's patience: the stream must end because shutdown
	// cancelled it, not because the drain window closed its connection.
	setDrainTimeout(t, time.Minute)
	stubMicSetting(t, func() (bool, error) { return true, nil })
	client, served := startServe(t)

	stream := openServedMicStream(t, client)
	// The greeting demand proves the handler is attached and parked in its loop.
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("mic stream never attached: %v", err)
	}

	shutdownAndWait(t, client, served)

	if _, err := stream.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("stream ended with %v, want Unavailable so the client reconnects", err)
	}
}

// TestServeShutdownAbandonsStuckRPCs covers the handler that never looks at
// its context (one parked in a backend call): the drain window closes its
// connection and the daemon exits anyway.
func TestServeShutdownAbandonsStuckRPCs(t *testing.T) {
	setDrainTimeout(t, 200*time.Millisecond)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	stubMicSetting(t, func() (bool, error) {
		once.Do(func() { close(entered) })
		<-release
		return false, nil
	})
	client, served := startServe(t)
	// Registered after startServe, so it runs first: the handler is let go
	// before the seam it is parked in is restored.
	t.Cleanup(func() { close(release) })

	openServedMicStream(t, client)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("mic handler never reached the config read")
	}

	shutdownAndWait(t, client, served)
}

// TestServeShutdownHardLimit: a shutdown that is still not done when the hard
// limit passes ends the process, whatever it was waiting for.
func TestServeShutdownHardLimit(t *testing.T) {
	setDrainTimeout(t, time.Minute) // the wedge: a drain that will not end
	origLimit, origExit := shutdownHardLimit, exitProcess
	exited := make(chan int, 1)
	shutdownHardLimit, exitProcess = 200*time.Millisecond, func(code int) { exited <- code }
	t.Cleanup(func() { shutdownHardLimit, exitProcess = origLimit, origExit })

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	stubMicSetting(t, func() (bool, error) {
		once.Do(func() { close(entered) })
		<-release
		return false, nil
	})
	client, served := startServe(t)
	openServedMicStream(t, client)
	<-entered

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Shutdown(sctx, &fleetgrpc.ShutdownRequest{Drain: true}); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case code := <-exited:
		if code == 0 {
			t.Fatal("the hard exit reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a wedged shutdown never hit the hard limit")
	}

	// Let the drain finish, so Serve is gone before the seams are restored.
	close(release)
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return once the stuck handler was released")
	}
}
