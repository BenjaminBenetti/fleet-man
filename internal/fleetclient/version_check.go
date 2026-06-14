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
// just connected to, returning whether it relaunched it (so the caller can
// re-settle its connection onto the new process). The decision is delegated to
// the pure, table-tested decideReconcile; this only performs the chosen action.
func reconcileServer(ctx context.Context, ep Endpoint, svc fleetgrpc.FleetServiceClient, reply *fleetgrpc.HelloReply, spawned bool) (restarted bool, err error) {
	cv := version.Version
	sv := reply.GetServerVersion()
	// Binary-mtime staleness only matters for a dev client; skip the stat otherwise.
	stale := cv == "" && serverIsStale(reply)

	switch action, decErr := decideReconcile(cv, sv, ep.IsLocal(), spawned, stale); action {
	case actionRestart:
		if err := restartServer(ctx, ep, svc, restartReason()); err != nil {
			return false, err
		}
		return true, nil
	case actionError:
		return false, decErr
	default:
		return false, nil
	}
}

// reconcileAction is the pure policy decision computed by decideReconcile.
type reconcileAction int

const (
	actionNone    reconcileAction = iota // the running server is acceptable as-is
	actionRestart                        // a LOCAL server must be drained + relaunched
	actionError                          // incompatible; cannot reconcile
)

// decideReconcile is the version/freshness policy expressed as a pure function,
// so every case is unit-testable without spawning a process. cv is this client's
// compiled-in version ("" = a dev build); sv is the server's reported version
// ("" = a dev build); isLocal = the server runs on this host; spawned = WE just
// started it; stale = (dev client only) this binary postdates the server's start.
//
//   - DEV client: ignore versions entirely — restart a local, pre-existing
//     server only when this freshly-built binary postdates it. This holds even
//     against a VERSIONED server, since one machine both tests dev builds and
//     runs the release, so build-time is the only reliable signal.
//   - VERSIONED client: a server reporting NO version is a dev build — replace it
//     locally, error on a remote one. The same numeric core is fine (a
//     pre-release counts as its release; see versionCore). A strictly-older local
//     server is replaced; a newer one (or any remote version mismatch) errors.
func decideReconcile(cv, sv string, isLocal, spawned, stale bool) (reconcileAction, error) {
	if cv == "" { // dev client
		if spawned || !isLocal || !stale {
			return actionNone, nil
		}
		return actionRestart, nil
	}

	// Versioned client.
	if sv == "" { // server is a dev build (or pre-version)
		if !isLocal {
			return actionError, fmt.Errorf("fleet server is a dev build but client is %s — restart the server", cv)
		}
		return actionRestart, nil
	}
	if versionCore(cv) == versionCore(sv) {
		return actionNone, nil
	}
	if !isLocal {
		return actionError, fmt.Errorf("fleet server is %s but client is %s — upgrade your client", sv, cv)
	}
	if versionLess(sv, cv) { // server older than this client
		return actionRestart, nil
	}
	return actionError, fmt.Errorf("fleet server is newer (%s) than this client (%s) — upgrade your client", sv, cv)
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
// reconcileServer (the version handshake) or to an explicit caller like
// RestartLocalServer (the manual "restart daemon" action). reason is logged by
// the server in its shutdown event.
func restartServer(ctx context.Context, ep Endpoint, svc fleetgrpc.FleetServiceClient, reason string) error {
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
		Reason: strptr(reason),
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

// versionCore strips an optional leading 'v' and any pre-release/build suffix
// (everything from the first '-'), leaving the dotted numeric core: so
// "v1.2.3-beta" and "v1.2.3" both yield "1.2.3". This keeps a pre-release from
// being treated as a different — or "newer" — version than its release.
func versionCore(v string) string {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	return v
}

// versionLess reports whether version a is strictly older than b, comparing only
// their numeric cores — pre-release/build suffixes are ignored (see versionCore),
// so v1.2.3-beta and v1.2.3 are NOT ordered relative to each other. A non-numeric
// component falls back to a string compare for that position.
func versionLess(a, b string) bool {
	as := strings.Split(versionCore(a), ".")
	bs := strings.Split(versionCore(b), ".")
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
