package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// jobcancel.go gives every lifecycle job its own cancellable context, and lets
// a destroy cancel the provisioning jobs (create, clone, rebuild) still working
// on its targets. Before this a create ran on the daemon's bare background
// context, so destroying a CREATING instance raced it: the destroy removed the
// record and workspace while the create went on to start a container nobody
// recorded. Now the destroy marks the targets StatusDeleting, cancels their
// jobs, and waits (bounded) for them to exit before tearing anything down; a
// cancelled run stops at its next step and removes any container it already
// started for a record being destroyed (internal/create/cancel.go). A run the
// daemon's shutdown cancelled keeps its container on the failed record.

var (
	// errInstanceDestroyed is the cancellation cause of a job whose instance is
	// being destroyed. A cancelled run reports "cancelled: instance destroyed".
	errInstanceDestroyed = errors.New("instance destroyed")
	// errDaemonShutdown is the cancellation cause of every job still running
	// when the daemon shuts down.
	errDaemonShutdown = errors.New("fleet daemon shutting down")
	// errJobFinished releases a job's context once its work has returned.
	errJobFinished = errors.New("job finished")
)

// destroyCancelWait bounds how long a destroy waits for the jobs it cancelled
// to exit. A backend call that takes no context (devcontainer up, docker
// commit) only notices the cancellation when it returns, so the wait is long
// enough for the usual case and bounded for a wedged one: past it the destroy
// proceeds, and the cancelled run still tears down its own container whenever
// its backend call finally returns. A var so tests can shorten it.
var destroyCancelWait = 60 * time.Second

// newJobContext derives a job's context from the daemon's background context.
// The job is cancelled with a cause naming why it stopped (errInstanceDestroyed
// from a destroy, errDaemonShutdown when parent ends at shutdown), so a run's
// failure reads as a reason rather than "context canceled". Values on parent
// carry over.
func newJobContext(parent context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	stop := context.AfterFunc(parent, func() { cancel(errDaemonShutdown) })
	return ctx, func(cause error) {
		stop()
		cancel(cause)
	}
}

// isProvisioningJob reports whether a job of this kind builds a container a
// destroy would race: the kinds a destroy cancels, and the kinds whose
// transitional status (creating, cloning, rebuilding) means "a job is working
// on this record".
func isProvisioningJob(k fleetgrpc.JobKind) bool {
	switch k {
	case fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE,
		fleetgrpc.JobKind_JOB_KIND_CLONE_INSTANCE,
		fleetgrpc.JobKind_JOB_KIND_REBUILD_INSTANCE:
		return true
	}
	return false
}

// provisioningJobs returns the in-flight provisioning jobs for fleetName's
// instance instanceName, or for every instance of the fleet when instanceName
// is empty.
func (m *jobManager) provisioningJobs(fleetName, instanceName string) []*job {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*job
	for _, j := range m.active {
		if !isProvisioningJob(j.summary.GetKind()) || j.summary.GetFleet() != fleetName {
			continue
		}
		if instanceName != "" && j.summary.GetInstance() != instanceName {
			continue
		}
		out = append(out, j)
	}
	return out
}

// hasProvisioningJob reports whether a provisioning job is in flight for the
// instance.
func (m *jobManager) hasProvisioningJob(fleetName, instanceName string) bool {
	return len(m.provisioningJobs(fleetName, instanceName)) > 0
}

// cancelProvisioning cancels, with cause, the in-flight provisioning jobs for
// fleetName/instanceName (every instance of the fleet when instanceName is
// empty) and returns them so the caller can wait for them to exit.
func (m *jobManager) cancelProvisioning(fleetName, instanceName string, cause error) []*job {
	jobs := m.provisioningJobs(fleetName, instanceName)
	for _, j := range jobs {
		j.cancel(cause)
	}
	return jobs
}

// awaitCancelledJobs waits for the jobs a destroy cancelled to exit, returning
// a warning for each one still running after destroyCancelWait. The destroy
// goes ahead either way: such a run tears down its own container once its
// backend call returns.
func awaitCancelledJobs(ctx context.Context, cancelled []*job) []string {
	if len(cancelled) == 0 {
		return nil
	}
	start := time.Now()
	stuck := awaitJobs(ctx, cancelled, destroyCancelWait)
	var warnings []string
	for _, cj := range stuck {
		warnings = append(warnings, fmt.Sprintf("%s job for %s/%s still running after %s; tearing down anyway",
			jobKindName(cj.summary.GetKind()), cj.summary.GetFleet(), cj.summary.GetInstance(), destroyCancelWait))
	}
	flog.Info("destroy: cancelled in-flight jobs", "fleet", cancelled[0].summary.GetFleet(), "count", len(cancelled), "stuck", len(stuck), "ms", flog.MillisSince(start))
	return warnings
}

// awaitJobs waits for every job to exit, up to limit or until ctx ends, and
// returns the ones still running.
func awaitJobs(ctx context.Context, jobs []*job, limit time.Duration) []*job {
	if len(jobs) == 0 {
		return nil
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for _, j := range jobs {
		select {
		case <-j.exited:
		case <-timer.C:
			return stillRunning(jobs)
		case <-ctx.Done():
			return stillRunning(jobs)
		}
	}
	return nil
}

func stillRunning(jobs []*job) []*job {
	var out []*job
	for _, j := range jobs {
		select {
		case <-j.exited:
		default:
			out = append(out, j)
		}
	}
	return out
}
