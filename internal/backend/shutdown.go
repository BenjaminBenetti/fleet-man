package backend

import (
	"context"
	"errors"
)

// ErrDaemonShutdown is the cancellation cause (context.Cause) of every job
// still running when the daemon shuts down.
var ErrDaemonShutdown = errors.New("fleet daemon shutting down")

// ErrCheckpointed marks a provisioning run a backend stopped at a checkpoint
// because the daemon is shutting down: the run's journal says what it had
// done, and the next daemon start settles its record from it (a Kubernetes
// create goes FAILED kubernetes:interrupted naming its step, exactly as after
// a daemon kill). The caller leaves the record as it is.
var ErrCheckpointed = errors.New("checkpointed for the next daemon start")

// ShutdownCut reports whether ctx ended because the daemon is shutting down.
func ShutdownCut(ctx context.Context) bool {
	return ctx.Err() != nil && errors.Is(context.Cause(ctx), ErrDaemonShutdown)
}
