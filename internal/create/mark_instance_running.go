package create

import (
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// markInstanceRunning reloads state, records the freshly-provisioned
// containerID on the named instance, flips it to StatusRunning, clears any
// prior error, and persists the change. It is the shared success-path update
// used by both Run and RunClone.
//
// State is reloaded here (rather than mutating an earlier snapshot) because the
// long provisioning steps in between may have raced other writers; loading
// fresh keeps those edits. A missing fleet/instance is silently skipped — the
// record is created by the caller before provisioning, so absence means it was
// removed concurrently and there is nothing to mark. The state.Load error is
// returned so a truly broken state file surfaces as a hard failure.
func markInstanceRunning(fleetName, instanceName, containerID string) error {
	st, err := state.Load()
	if err != nil {
		return err
	}
	if f, ok := st.Fleets[fleetName]; ok {
		if instance, err := f.GetInstance(instanceName); err == nil {
			instance.ContainerID = containerID
			instance.Status = fleet.StatusRunning
			instance.Error = ""
		}
	}
	return state.Save(st)
}
