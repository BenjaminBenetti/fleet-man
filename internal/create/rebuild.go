package create

import (
	"context"
	"fmt"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
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
//  4. Hand off to the backend's Rebuild, which tears down the old container and
//     provisions a fresh one from the (possibly edited) devcontainer config,
//     PRESERVING the workspace (the git checkout and uncommitted edits survive).
//  5. Re-run the full post-Up provisioning so the rebuilt container is
//     equivalent to a freshly created one — unlike a clone, the fresh container
//     carries none of the prior fleet-launch / dotfiles / hook state.
//  6. Persist the new ContainerID (it changes for devcontainer) and flip back
//     to StatusRunning.
//
// ctx is the daemon job's: cancelling it (a destroy of the instance, or the
// daemon shutting down) stops the rebuild at the next step, tearing down a
// container it already started (see cancel.go).
func RunRebuild(ctx context.Context, fleetName, instanceName string, verbose bool) (err error) {
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

	instanceBackend := newInstanceBackend(inst, verbose)
	if !instanceBackend.SupportsRebuild() {
		err := fmt.Errorf("backend %q does not support rebuild", inst.Backend)
		setFailed(fleetName, instanceName, err)
		return err
	}

	backend.ReportStep(ctx, fleet.StepResolveMounts, "")
	resolvedMounts, err := resolveProvisionMounts(ctx, instanceBackend, fleetName, instanceName)
	if err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}

	if err := checkCancelled(ctx); err != nil {
		setFailed(fleetName, instanceName, err)
		return err
	}
	// RebuildContext hands the job's ctx (cancellation, JobLog) to a backend
	// that takes one, and falls back to the plain Rebuild for the rest.
	backend.ReportStep(ctx, fleet.StepBackendUp, "rebuild "+string(inst.Backend))
	result, err := backend.RebuildContext(ctx, instanceBackend, inst.ContainerID, backend.UpRequest{WorkspaceDir: inst.WorkspaceDir, Mounts: resolvedMounts.Mounts})
	if err != nil {
		// A ctx-aware backend stopped by the cancellation reports why.
		err = cancelledOr(ctx, err)
		setFailed(fleetName, instanceName, err)
		return err
	}
	// A plain Rebuild takes no context: stop a rebuild the run was cancelled
	// during. Only a destroy's cancellation tears the container down
	// (abandonRun): on a shutdown it is the instance's only container (for
	// codespaces, the codespace itself), so the failed record keeps it.
	if err := stopIfCancelled(ctx, instanceBackend, fleetName, instanceName, result.ContainerID); err != nil {
		return err
	}

	// Re-run the post-Up provisioning so the fresh container ends up equivalent
	// to a newly created one. All best-effort (warnings, never fatal).
	finishProvision(ctx, instanceBackend, fleetName, instanceName, inst.WorkspaceDir, result.ContainerID, resolvedMounts.Symlinks)

	backend.ReportStep(ctx, fleet.StepMarkRunning, "")
	if err := commitRunning(ctx, instanceBackend, fleetName, instanceName, result.ContainerID); err != nil {
		return err
	}
	flog.Info("instance rebuilt", "fleet", fleetName, "instance", instanceName, "container", result.ContainerID, "ms", flog.MillisSince(start))
	return nil
}
