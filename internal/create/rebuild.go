package create

import (
	"fmt"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// RunRebuild recreates an existing instance's container in place. The instance
// record must already exist (the server pre-flips it to StatusRebuilding so the
// teardown is visible in the UI while the backend works).
//
// The flow:
//
//  1. Look up the instance from state and build its backend.
//  2. Refuse early if the backend does not support rebuild so the caller
//     surfaces a clear error instead of partial work.
//  3. Resolve the same mounts a fresh Up would (control socket, shared
//     buildkit, agent-state mounts) — they must be re-applied to the new
//     container.
//  4. Hand off to backend.Rebuild, which tears down the old container and
//     provisions a fresh one from the (possibly edited) devcontainer config,
//     PRESERVING the workspace (the git checkout and uncommitted edits survive).
//  5. Re-run the full post-Up provisioning so the rebuilt container is
//     equivalent to a freshly created one — unlike a clone, the fresh container
//     carries none of the prior fleet-launch / dotfiles / hook state.
//  6. Persist the new ContainerID (it changes for devcontainer) and flip back
//     to StatusRunning.
func RunRebuild(fleetName, instanceName string, verbose bool) (err error) {
	start := time.Now()
	flog.Info("instance rebuild started", "fleet", fleetName, "instance", instanceName)
	// Failure outcome (with elapsed time) is logged from one place; setFailed
	// only annotates state. Success is logged inline at the end.
	defer func() {
		if err != nil {
			flog.Error("instance rebuild failed", "fleet", fleetName, "instance", instanceName, "ms", flog.MillisSince(start), "err", err)
		}
	}()

	st, err := state.Load()
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}
	f, ok := st.Fleets[fleetName]
	if !ok {
		err := fmt.Errorf("fleet %q not found", fleetName)
		setFailed(fleetName, instanceName, err)
		return err
	}
	inst, err := f.GetInstance(instanceName)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	instanceBackend := backendutil.NewForInstance(inst, verbose)
	if !instanceBackend.SupportsRebuild() {
		err := fmt.Errorf("backend %q does not support rebuild", inst.Backend)
		setFailed(fleetName, instanceName, err)
		return err
	}

	resolvedMounts, err := resolveProvisionMounts(instanceBackend, fleetName, instanceName)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	result, err := instanceBackend.Rebuild(inst.ContainerID, inst.WorkspaceDir, resolvedMounts.Mounts)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	// Re-run the post-Up provisioning so the fresh container ends up equivalent
	// to a newly created one. All best-effort (warnings, never fatal).
	finishProvision(instanceBackend, fleetName, instanceName, inst.WorkspaceDir, result.ContainerID, resolvedMounts.Symlinks)

	if err := markInstanceRunning(fleetName, instanceName, result.ContainerID); err != nil {
		return err
	}
	flog.Info("instance rebuilt", "fleet", fleetName, "instance", instanceName, "container", result.ContainerID, "ms", flog.MillisSince(start))
	return nil
}
