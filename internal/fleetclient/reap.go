package fleetclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
)

// reap.go deals with a local daemon that exists but cannot be used: one still
// on its way out, or wedged. The daemon holds its lifetime lock
// (fleetpaths.ServerLockPath) for as long as its process lives, so the lock —
// not the socket — says whether a daemon process is there, and a replacement
// can only start once it is free. Everything here runs under the spawn lock.

// Vars so tests need not wait them out.
var (
	// unresponsiveGrace is how long a daemon that holds the lifetime lock may
	// stay silent before it is killed: enough for one that is starting or
	// stopping to finish, and for a busy one to answer.
	unresponsiveGrace = 5 * time.Second
	// shutdownGrace is how long a daemon that was asked to shut down gets to
	// exit before it is killed.
	shutdownGrace = 8 * time.Second
	// killWait is how long each signal gets to take effect.
	killWait = 2 * time.Second
)

// serverLockHeld reports whether a daemon process holds the lifetime lock.
func serverLockHeld() bool {
	f, err := os.Open(fleetpaths.ServerLockPath())
	if err != nil {
		return false
	}
	defer f.Close()
	// Shared, so the probe only ever conflicts with the daemon's exclusive
	// hold; a daemon starting at this instant retries past it.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// clearServer makes room for a new daemon: it waits up to grace for the
// process holding the lifetime lock to exit, and kills it if it has not. With
// mayRevive the daemon is pinged meanwhile, and one that answers after all is
// kept (revived). killed describes what was killed, for the new daemon's log.
func clearServer(ctx context.Context, ep Endpoint, grace time.Duration, mayRevive bool) (revived bool, killed string, err error) {
	deadline := time.Now().Add(grace)
	for serverLockHeld() {
		if mayRevive && pingOK(ep) {
			return true, "", nil
		}
		if !time.Now().Before(deadline) {
			pids, err := killServer(ctx)
			if err != nil {
				return false, "", err
			}
			return false, fmt.Sprintf("killed pid %v after %s", pids, grace), nil
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false, "", nil
}

// killServer force-stops whatever holds the lifetime lock: SIGTERM, then
// SIGKILL for a daemon that ignores it (one wedged mid-shutdown has already
// spent its signal).
func killServer(ctx context.Context) ([]int, error) {
	pids, err := lockHolders(fleetpaths.ServerLockPath())
	if err != nil {
		return nil, fmt.Errorf("fleet server is not responding and its process could not be looked up: %w", err)
	}
	if len(pids) == 0 {
		return nil, fmt.Errorf("fleet server is not responding and its process was not found: kill it, then retry")
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		for _, pid := range pids {
			_ = syscall.Kill(pid, sig)
		}
		deadline := time.Now().Add(killWait)
		for time.Now().Before(deadline) {
			if !serverLockHeld() {
				return pids, nil
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return nil, fmt.Errorf("fleet server (pid %v) is not responding and could not be killed", pids)
}
