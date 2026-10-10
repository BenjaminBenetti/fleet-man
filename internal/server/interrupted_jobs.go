package server

import (
	"errors"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// interrupted_jobs.go reconciles the records a previous daemon left mid-job.
// Lifecycle jobs live in this process's memory, so a daemon that exits (an
// upgrade, a crash, a kill) while a create, clone or rebuild runs leaves its
// record CREATING, CLONING or REBUILDING with nothing left to finish it. Such a
// record used to stay wedged in that status for good: the TUI and the server's
// transitional guard both refuse to act on it. At start, every such record
// with no live job becomes FAILED with interruptedByRestart; the run is never
// resumed. Rebuild is the retry (a FAILED record is not transitional), and
// destroy removes it.

// interruptedByRestart is the error recorded on a record reconciled at start.
const interruptedByRestart = "interrupted by daemon restart"

// errNothingInterrupted aborts the reconcile's state.Update when no record
// changed, so a clean start does not rewrite state.json.
var errNothingInterrupted = errors.New("no interrupted records")

// failInterruptedJobs marks every interrupted record FAILED. Serve calls it
// before it accepts RPCs or starts the scheduler, so no job of this daemon can
// exist yet; the live-job check keeps it correct if it ever runs later.
func (s *service) failInterruptedJobs() {
	var failed []string
	err := state.Update(func(st *state.State) error {
		failed = markInterrupted(st, s.jobs.hasProvisioningJob)
		if len(failed) == 0 {
			return errNothingInterrupted
		}
		return nil
	})
	if err != nil && !errors.Is(err, errNothingInterrupted) {
		flog.Warn("reconcile interrupted jobs failed", "err", err)
		return
	}
	for _, key := range failed {
		flog.Warn("instance job interrupted by daemon restart; marked failed", "instance", key)
		if fleetName, instanceName, ok := strings.Cut(key, "/"); ok {
			appendCreationLogError(fleetName, instanceName, interruptedByRestart)
		}
	}
}

// markInterrupted flips each CREATING, CLONING or REBUILDING record for which
// live reports no provisioning job to FAILED, returning the "fleet/instance"
// keys it changed.
func markInterrupted(st *state.State, live func(fleetName, instanceName string) bool) []string {
	var failed []string
	for fleetName, f := range st.Fleets {
		if f == nil {
			continue
		}
		for _, inst := range f.Instances {
			switch inst.Status {
			case fleet.StatusCreating, fleet.StatusCloning, fleet.StatusRebuilding:
			default:
				continue
			}
			if live(fleetName, inst.Name) {
				continue
			}
			inst.Status = fleet.StatusFailed
			inst.Error = interruptedByRestart
			failed = append(failed, fleetName+"/"+inst.Name)
		}
	}
	return failed
}
