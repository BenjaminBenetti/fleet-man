package fleetclient

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ensureServerLocal makes sure a local fleet server is reachable, spawning one
// under a single-winner lock if not. Safe under N racing clients: the flock
// serializes them and each re-checks reachability before spawning, so at most
// one spawn happens — and the server's own lifetime lock guarantees one server
// even if two ever race past here.
func ensureServerLocal(ctx context.Context, ep Endpoint) error {
	lockFD, err := acquireSpawnLock(ctx)
	if err != nil {
		return fmt.Errorf("acquire spawn lock: %w", err)
	}
	defer releaseSpawnLock(lockFD)

	// Another client may have spawned the server while we waited for the lock.
	if pingOK(ep) {
		return nil
	}
	if err := startServerProcess(); err != nil {
		return fmt.Errorf("spawn fleet server: %w", err)
	}
	return waitReady(ctx, ep, 5*time.Second)
}

// startServerProcess fork-execs `fleet server` detached, so it outlives the
// client that spawned it.
func startServerProcess() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "server")
	cmd.Env = os.Environ()
	// Setsid detaches from this client's session/controlling terminal, so
	// closing the client's terminal won't SIGHUP the server.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
		defer devnull.Close()
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Detach: never Wait; release the OS process handle.
	return cmd.Process.Release()
}

// waitReady polls until the server answers Hello or the deadline passes.
func waitReady(ctx context.Context, ep Endpoint, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if pingOK(ep) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("fleet server did not become ready within %s", within)
}

// pingOK reports whether a fleet server answers Hello at ep right now.
func pingOK(ep Endpoint) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := grpc.NewClient(ep.Target(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return false
	}
	defer conn.Close()
	_, err = fleetgrpc.NewFleetServiceClient(conn).Hello(ctx, &fleetgrpc.HelloRequest{ClientVersion: version.Version})
	return err == nil
}

// acquireSpawnLock takes the spawn lock, waiting (bounded) for another client
// that is mid-spawn. The returned *os.File must stay open to hold the lock.
func acquireSpawnLock(ctx context.Context) (*os.File, error) {
	f, err := os.OpenFile(fleetpaths.SpawnLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return f, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("timed out waiting for spawn lock")
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func releaseSpawnLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
