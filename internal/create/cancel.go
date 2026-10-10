package create

import (
	"context"
	"errors"
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// cancel.go is how a provisioning run (create, clone, rebuild) honours its
// job's cancellation: a destroy of the instance, or the daemon shutting down.
// Backend calls such as Up, Clone and Rebuild take no context, so a run checks
// between steps instead. A container that a step produced after the
// cancellation is torn down here: nothing will record it (the record is being
// destroyed, or the run is reported as failed), so leaving it would leak it.

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

// discardIfCancelled is checkCancelled after a step that produced containerID:
// when the run was cancelled meanwhile, the container is torn down first.
func discardIfCancelled(ctx context.Context, b backend.Backend, fleetName, instanceName, containerID string) error {
	err := checkCancelled(ctx)
	if err != nil {
		discardContainer(b, fleetName, instanceName, containerID, err)
	}
	return err
}

// isAbandonedRun reports whether err stopped a run because of a cancellation or
// a destroyed record, i.e. a run whose container nobody will record.
func isAbandonedRun(err error) bool {
	return errors.Is(err, errCancelled) || errors.Is(err, errAbandoned)
}

// discardContainer tears down the container of an abandoned run. Best-effort:
// a failure is logged, and the run still reports why it stopped.
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
// job was cancelled, or its record is gone or being deleted — the container is
// torn down instead and the run fails with the reason.
func commitRunning(ctx context.Context, b backend.Backend, fleetName, instanceName, containerID string) error {
	err := markInstanceRunning(ctx, fleetName, instanceName, containerID)
	if err != nil && isAbandonedRun(err) {
		discardContainer(b, fleetName, instanceName, containerID, err)
		setFailed(fleetName, instanceName, err)
	}
	return err
}
