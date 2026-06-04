package create

import (
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// setFailed marks the named instance StatusFailed and records origErr's
// message on it, persisting the change. State load/save errors are swallowed:
// it is a best-effort failure annotation invoked from error paths that have
// already decided to bail, so it must not mask the original error with a
// secondary one. The failure itself (with timing) is logged by the caller
// (Run / RunClone), so setFailed does not log.
func setFailed(fleetName, instanceName string, origErr error) {
	// Atomic RMW so a concurrent provisioning job marking a different instance
	// running cannot clobber this failure annotation (or vice versa).
	_ = state.Update(func(st *state.State) error {
		if f, ok := st.Fleets[fleetName]; ok {
			if instance, err := f.GetInstance(instanceName); err == nil {
				instance.Status = fleet.StatusFailed
				instance.Error = origErr.Error()
			}
		}
		return nil
	})
}
