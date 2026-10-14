package create

import (
	"context"
	"errors"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// cancel.go is how a provisioning run (create, clone, rebuild) honours its
// job's cancellation: a destroy of the instance, or the daemon shutting down.
// Backend calls such as Up, Clone and Rebuild take no context, so a run checks
// between steps instead. What becomes of a container a step produced is
// decided under the state lock (abandonRun): a destroy's container is torn
// down here, since nothing will record it; a shutdown's stays on the failed
// record, since tearing it down could lose the instance's data.

// errCancelled marks a run stopped by its job's cancellation. The cancellation
// cause (why the job was cancelled) is wrapped alongside it.
var errCancelled = errors.New("cancelled")

// errAbandoned marks a run whose instance record was destroyed, or marked for
// destruction, while it provisioned. It is the state-side twin of
// errCancelled: checked under the state lock as the run commits, so a destroy
// that lands between the last context check and the commit is still seen.
var errAbandoned = errors.New("instance was destroyed while it was being provisioned")

// checkCancelled returns the run's cancellation as an error, or nil while the
// run may continue.
func checkCancelled(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", errCancelled, context.Cause(ctx))
}

// cancelledOr returns err, a failed step's error, or the run's cancellation
// when the run was cancelled meanwhile: a git clone or copy killed by the
// cancellation fails with "signal: killed", and why it was killed is the
// failure to report.
func cancelledOr(ctx context.Context, err error) error {
	if cerr := checkCancelled(ctx); cerr != nil {
		return cerr
	}
	return err
}

// stopIfCancelled is checkCancelled after a step that produced containerID:
// when the run was cancelled meanwhile, it ends the run there (abandonRun).
func stopIfCancelled(ctx context.Context, b backend.Backend, fleetName, instanceName, containerID string) error {
	err := checkCancelled(ctx)
	if err != nil {
		abandonRun(b, fleetName, instanceName, containerID, err)
	}
	return err
}

// isAbandonedRun reports whether err stopped a run because of a cancellation or
// a destroyed record.
func isAbandonedRun(err error) bool {
	return errors.Is(err, errCancelled) || errors.Is(err, errAbandoned)
}

// abandonRun ends a run that why stopped after a step produced containerID
// ("" for none), deciding under the state lock what becomes of the container:
//
//   - the record is gone or StatusDeleting (a destroy removed it, or marked it
//     before cancelling the job): the destroy has not seen this container and
//     will not, so it is torn down here;
//   - any other record (the daemon shutting down cancelled the job): the
//     record keeps the container and is failed with why. Tearing it down could
//     lose the instance's data (a codespaces rebuild returns the codespace
//     itself, and a devcontainer's named volumes go with its last container),
//     while a recorded container is what rebuild retries and destroy removes.
//
// A state that cannot be read keeps the container: a leak over a data loss.
func abandonRun(b backend.Backend, fleetName, instanceName, containerID string, why error) {
	discard := false
	_ = state.Update(func(st *state.State) error {
		var instance *fleet.Instance
		if f, ok := st.Fleets[fleetName]; ok {
			instance, _ = f.GetInstance(instanceName)
		}
		if instance == nil || instance.Status == fleet.StatusDeleting {
			discard = true
			return errAbandoned // nothing to write
		}
		if containerID != "" {
			instance.ContainerID = containerID
		}
		instance.Status = fleet.StatusFailed
		instance.Error = why.Error()
		return nil
	})
	if discard {
		discardContainer(b, fleetName, instanceName, containerID, why)
	}
}

// discardContainer tears down the container of a run its destroy abandoned.
// Best-effort: a failure is logged, and the run still reports why it stopped.
func discardContainer(b backend.Backend, fleetName, instanceName, containerID string, why error) {
	if containerID == "" {
		return
	}
	if err := b.Down(containerID); err != nil {
		flog.Warn("abandoned provisioning: container teardown failed", "fleet", fleetName, "instance", instanceName, "container", containerID, "why", why, "err", err)
		return
	}
	flog.Info("abandoned provisioning: container torn down", "fleet", fleetName, "instance", instanceName, "container", containerID, "why", why)
}

// commitRunning records the run's container on the instance and flips it to
// running (markInstanceRunning). When the run turns out to be abandoned — its
// job was cancelled, or its record is gone or being deleted — abandonRun ends
// it instead, and the run fails with the reason.
func commitRunning(ctx context.Context, b backend.Backend, fleetName, instanceName, containerID string) error {
	err := markInstanceRunning(ctx, fleetName, instanceName, containerID)
	if err != nil && isAbandonedRun(err) {
		abandonRun(b, fleetName, instanceName, containerID, err)
	}
	return err
}
