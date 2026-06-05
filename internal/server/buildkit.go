package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/buildkit"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// ensureBuildkitServer is the re-ensure seam (a package var so the TUI-connect
// sweep can be exercised in tests without docker). Mirrors stopBuildkitServer in
// jobs.go.
var ensureBuildkitServer = buildkit.EnsureSharedServer

// ensureConfiguredBuildkitServers re-ensures the shared buildkit server for
// every fleet that has the feature enabled AND actually uses it. It is the
// fire-and-forget reconciliation kicked off when a TUI connects (see
// service.onTUIConnected), recovering from a server that was killed externally
// or lost to a host reboot while no instance create/clone/start would otherwise
// have revived it.
//
// Gating mirrors the per-instance paths: a fleet is only ensured when it has at
// least one instance on a backend that SupportsCustomMounts (devcontainer) — so
// empty fleets and cloud-only fleets never spin up an unused host container.
// Best-effort throughout: a state-load or per-fleet failure is logged, never
// surfaced (the caller does not wait on the result). EnsureSharedServer is
// idempotent and per-fleet serialized, so this is safe to run concurrently with
// instance lifecycle jobs and with itself.
//
// ctx bounds the sweep: it is checked between fleets so the sweep stops promptly
// on server shutdown or the caller's timeout (it does not interrupt an
// individual EnsureSharedServer mid-docker-call).
func ensureConfiguredBuildkitServers(ctx context.Context) {
	st, err := state.Load()
	if err != nil {
		flog.Warn("buildkit reconcile: load state failed", "err", err)
		return
	}
	for name, f := range st.Fleets {
		if ctx.Err() != nil {
			return
		}
		if f == nil || !f.Settings.BuildkitServer {
			continue
		}
		if !fleetHasCustomMountInstance(f) {
			continue
		}
		if _, err := ensureBuildkitServer(name); err != nil {
			flog.Warn("buildkit reconcile: ensure failed", "fleet", name, "err", err)
		}
	}
}

// fleetHasCustomMountInstance reports whether any instance in the fleet runs on
// a backend that honors custom mounts (devcontainer). An empty Backend defaults
// to devcontainer, matching backendutil.New / jobDownInstance. Cloud-only and
// instance-less fleets return false so they are skipped by the reconcile.
func fleetHasCustomMountInstance(f *fleet.Fleet) bool {
	for _, inst := range f.Instances {
		if inst == nil {
			continue // tolerate a malformed/hand-edited state.json
		}
		if backendutil.New(inst.Backend, false).SupportsCustomMounts() {
			return true
		}
	}
	return false
}
