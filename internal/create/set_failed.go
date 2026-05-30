package create

import (
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// setFailed marks the named instance StatusFailed and records origErr's
// message on it, persisting the change. State load/save errors are swallowed:
// it is a best-effort failure annotation invoked from error paths that have
// already decided to bail, so it must not mask the original error with a
// secondary one.
func setFailed(fleetName, instanceName string, origErr error) {
	st, err := state.Load()
	if err != nil {
		return
	}
	if f, ok := st.Fleets[fleetName]; ok {
		if instance, err := f.GetInstance(instanceName); err == nil {
			instance.Status = fleet.StatusFailed
			instance.Error = origErr.Error()
		}
	}
	_ = state.Save(st)
}
