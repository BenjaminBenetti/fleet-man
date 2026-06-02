// Package server is the fleet daemon (fleetd): the single, long-running,
// per-user process that owns ~/.fleet and (in later phases) all backend
// operations. CLI and TUI clients talk to it over gRPC on a unix socket.
//
// This package is SERVER-ONLY. Client code (internal/cli, internal/tui,
// internal/fleetclient) must never import it — except internal/cli/server.go,
// which is the entrypoint that runs the daemon.
package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
	"google.golang.org/grpc"
)

// Serve runs the fleet server until ctx is cancelled (signal) or a Shutdown RPC
// arrives. It returns nil when another server already holds the lifetime lock
// (this invocation is a redundant spawn), so racing spawns exit cleanly.
func Serve(ctx context.Context) error {
	dir := fleetpaths.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create fleet dir: %w", err)
	}

	// Lifetime lock: exactly one fleet server per user. If another server holds
	// it, we're a redundant spawn (a client double-spawned, or a stale socket
	// fooled someone) — exit cleanly so the caller uses the existing server.
	lockFD, err := acquireServerLock()
	if err != nil {
		flog.Info("fleet server already running; redundant spawn exiting", "pid", os.Getpid())
		return nil
	}
	defer releaseLock(lockFD)

	// Holding the lifetime lock proves no other server exists, so any leftover
	// socket file is stale and safe to remove (net.Listen fails if it exists).
	sock := fleetpaths.SocketPath()
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket: %w", err)
	}

	lis, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", sock, err)
	}
	// 0600: host-local, per-user. Unlike the bind-mounted control socket,
	// nothing cross-UID connects here.
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = lis.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}

	svc := newService()
	grpcServer := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(grpcServer, svc)

	writeVersionFile()
	defer func() { _ = os.Remove(fleetpaths.VersionFilePath()) }()

	flog.Info("fleet server started", "pid", os.Getpid(), "socket", sock, "version", versionOrDev())

	serveErr := make(chan error, 1)
	go func() { serveErr <- grpcServer.Serve(lis) }()

	select {
	case <-ctx.Done():
		flog.Info("fleet server stopping (signal)", "pid", os.Getpid())
		grpcServer.GracefulStop()
	case <-svc.shutdownCh:
		flog.Info("fleet server stopping (Shutdown RPC)", "pid", os.Getpid())
		grpcServer.GracefulStop()
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
	}
	return nil
}

// acquireServerLock takes the exclusive flock held for the server's whole
// lifetime. The returned *os.File must stay open to hold the lock.
//
// It retries briefly rather than failing on first contention: during a restart
// the outgoing server releases this lock a moment after it stops serving, and
// the incoming server (spawned once the old one stopped answering) should win it
// rather than exit as "redundant". A genuinely-redundant spawn (another healthy
// server holding the lock the whole window) simply waits it out, then exits.
func acquireServerLock() (*os.File, error) {
	f, err := os.OpenFile(fleetpaths.ServerLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			return f, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, lockErr
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// writeVersionFile records the running version as a cheap pre-dial hint. It is
// never authoritative — clients verify via the Hello handshake.
func writeVersionFile() {
	_ = os.WriteFile(fleetpaths.VersionFilePath(), []byte(versionOrDev()), 0o600)
}

func versionOrDev() string {
	if version.Version == "" {
		return "dev"
	}
	return version.Version
}
