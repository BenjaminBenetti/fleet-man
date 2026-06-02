package fleetclient

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
)

// reconcileServer enforces the version/freshness policy against the server we
// just connected to, returning whether it relaunched the server (so the caller
// can re-settle its connection onto the new process).
//
//   - DEV client (no compiled-in version): version strings can't tell us whether
//     a PRE-EXISTING server runs the freshly-built code, so we fall back to the
//     binary's mtime: if this executable is newer than the server's start time,
//     the server is stale and we replace it. This makes local dev fool-proof —
//     every rebuild's first command transparently gets a fresh server — WITHOUT
//     thrashing (an unchanged binary, or a server we just spawned, is left
//     alone, so repeated/concurrent commands don't restart needlessly). A remote
//     server can't be relaunched.
//   - VERSIONED client: same version is fine; a strictly-newer client relaunches
//     a too-old LOCAL server; an older client (or any remote mismatch) errors.
func reconcileServer(ctx context.Context, ep Endpoint, svc fleetgrpc.FleetServiceClient, reply *fleetgrpc.HelloReply, spawned bool) (restarted bool, err error) {
	cv := version.Version
	sv := reply.GetServerVersion()

	if cv == "" { // dev client: use binary freshness instead of version strings
		// A server we just spawned is, by definition, the current binary.
		if spawned || !ep.IsLocal() {
			return false, nil
		}
		if !serverIsStale(reply) {
			return false, nil
		}
		if err := restartServer(ctx, ep, svc); err != nil {
			return false, err
		}
		return true, nil
	}

	// Versioned client.
	if sv == "" || cv == sv {
		return false, nil
	}
	if !ep.IsLocal() {
		return false, fmt.Errorf("fleet server is %s but client is %s — upgrade your client", sv, cv)
	}
	if versionLess(sv, cv) { // server older than this client
		if err := restartServer(ctx, ep, svc); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, fmt.Errorf("fleet server is newer (%s) than this client (%s) — upgrade your client", sv, cv)
}

// serverIsStale reports whether the running server predates this client binary —
// i.e. the binary was (re)built after the server started, so the server is
// running old code. Used only for dev builds. On any uncertainty it returns
// false (leave the server alone) rather than risk thrashing, EXCEPT when the
// server reports no start time, which we treat as stale.
func serverIsStale(reply *fleetgrpc.HelloReply) bool {
	started := reply.GetStartedAt()
	if started == nil {
		return true
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	fi, err := os.Stat(self)
	if err != nil {
		return false
	}
	return fi.ModTime().After(started.AsTime())
}

// restartServer drains the running server and relaunches it from the current
// binary, under the single-winner spawn lock so racing clients cause exactly one
// restart at a time. It is a pure mechanic — the decision to restart belongs to
// reconcileServer.
func restartServer(ctx context.Context, ep Endpoint, svc fleetgrpc.FleetServiceClient) error {
	lockFD, err := acquireSpawnLock(ctx)
	if err != nil {
		return err
	}
	defer releaseSpawnLock(lockFD)

	// Ask the current server to drain and exit. Best-effort: it may already be
	// gone (a racing client restarted it while we waited for the lock), in which
	// case Shutdown errors and we just make sure one is up below.
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, _ = svc.Shutdown(sctx, &fleetgrpc.ShutdownRequest{
		Drain:  true,
		Reason: strptr(restartReason()),
	})
	cancel()

	// Wait for the old server to stop serving (closing its listener unlinks the
	// socket), then spawn the new one. We already hold the spawn lock, so spawn
	// directly rather than via ensureServerLocal (which would re-acquire it).
	stopDeadline := time.Now().Add(15 * time.Second)
	for pingOK(ep) && time.Now().Before(stopDeadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if err := startServerProcess(); err != nil {
		return fmt.Errorf("relaunch server: %w", err)
	}
	return waitReady(ctx, ep, 5*time.Second)
}

func restartReason() string {
	if version.Version == "" {
		return "dev client: replacing stale server with freshly-built binary"
	}
	return fmt.Sprintf("client %s newer than server", version.Version)
}

func strptr(s string) *string { return &s }

// versionLess reports whether version a is strictly older than b. Both are
// dotted numeric versions with an optional leading 'v' (e.g. v1.2.3). A
// non-numeric component falls back to a string compare for that position.
func versionLess(a, b string) bool {
	as := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bs := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var ai, bi string
		if i < len(as) {
			ai = as[i]
		}
		if i < len(bs) {
			bi = bs[i]
		}
		an, aerr := strconv.Atoi(ai)
		bn, berr := strconv.Atoi(bi)
		if aerr == nil && berr == nil {
			if an != bn {
				return an < bn
			}
			continue
		}
		if ai != bi {
			return ai < bi
		}
	}
	return false
}
