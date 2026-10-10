package create

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// markInstanceRunning reloads state, records the freshly-provisioned
// containerID on the named instance, flips it to StatusRunning, clears any
// prior error, and persists the change. It is the shared success-path update
// used by Run, RunClone and RunRebuild (through commitRunning).
//
// State is reloaded here (rather than mutating an earlier snapshot) because the
// long provisioning steps in between may have raced other writers; loading
// fresh keeps those edits. The state.Load error is returned so a truly broken
// state file surfaces as a hard failure.
//
// A run that was abandoned meanwhile commits nothing: when ctx is cancelled the
// cancellation is returned, and when the record is gone or StatusDeleting (a
// destroy removed it, or marked it before cancelling this run's job) it returns
// errAbandoned. Both checks run under the state lock, so a destroy either sees
// the container on the record (and tears it down itself) or the run sees the
// destroy (and tears it down here, in commitRunning) — never neither.
func markInstanceRunning(ctx context.Context, fleetName, instanceName, containerID string) error {
	// Atomic RMW (load fresh, mutate just this instance, save) so the long
	// provisioning steps that ran before this point can't have their concurrent
	// writers clobbered, and we can't clobber them.
	return state.Update(func(st *state.State) error {
		if err := checkCancelled(ctx); err != nil {
			return err
		}
		f, ok := st.Fleets[fleetName]
		if !ok {
			return errAbandoned
		}
		instance, err := f.GetInstance(instanceName)
		if err != nil || instance.Status == fleet.StatusDeleting {
			return errAbandoned
		}
		instance.ContainerID = containerID
		instance.Status = fleet.StatusRunning
		instance.Error = ""
		return nil
	})
}
