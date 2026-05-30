package create

import (
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	mountresolver "github.com/BenjaminBenetti/fleet-man/internal/mounts/resolver"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// resolveCustomMounts looks up the fleet's persisted settings and
// translates them into a Resolved bundle (mounts + symlinks). Returns
// a zero-value Resolved when the backend does not advertise
// SupportsCustomMounts so callers avoid wasted host-side preparation
// for cloud-managed backends.
//
// State load failures are tolerated: if state.json cannot be read the
// instance is still allowed to provision, just without any custom
// mounts. (The instance record we are about to fill in is loaded from
// the same file in the success path immediately after, which will
// surface a hard error then if the file is truly broken.)
func resolveCustomMounts(instanceBackend backend.Backend, fleetName string) (mountresolver.Resolved, error) {
	if !instanceBackend.SupportsCustomMounts() {
		return mountresolver.Resolved{}, nil
	}
	st, err := state.Load()
	if err != nil {
		return mountresolver.Resolved{}, nil
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		return mountresolver.Resolved{}, nil
	}
	return mountresolver.Resolve(fleetName, f.Settings)
}
