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
//
// Kubernetes records go by their job journals (kube_reconcile.go): an
// interrupted rebuild is claimed for an undo that keeps it REBUILDING until
// it is done, and the others fail with kubernetes:interrupted, naming the
// job and step the journal recorded.
func (s *service) failInterruptedJobs() {
	s.kubeRecovery.load()
	var failed []interruptedRecord
	err := state.Update(func(st *state.State) error {
		s.kubeRecovery.claim(st)
		live := func(fleetName, instanceName string) bool {
			return s.jobs.hasProvisioningJob(fleetName, instanceName) || s.kubeRecovery.claims(fleetName, instanceName)
		}
		failed = markInterrupted(st, live, s.kubeRecovery.interruptedError)
		if len(failed) == 0 {
			return errNothingInterrupted
		}
		return nil
	})
	if err != nil && !errors.Is(err, errNothingInterrupted) {
		flog.Warn("reconcile interrupted jobs failed", "err", err)
		return
	}
	for _, f := range failed {
		flog.Warn("instance job interrupted by daemon restart; marked failed", "instance", f.key)
		if fleetName, instanceName, ok := strings.Cut(f.key, "/"); ok {
			appendCreationLogError(fleetName, instanceName, f.err)
		}
	}
	s.kubeRecovery.settle()
}

// interruptedRecord is a record failed at start and the error it got.
type interruptedRecord struct {
	key string // "fleet/instance"
	err string
}

// markInterrupted flips each CREATING, CLONING or REBUILDING record for which
// live reports no provisioning job to FAILED, with the error why gives it
// (nil: interruptedByRestart), returning the records it changed.
func markInterrupted(st *state.State, live func(fleetName, instanceName string) bool, why func(fleetName string, inst *fleet.Instance) string) []interruptedRecord {
	var failed []interruptedRecord
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
			msg := interruptedByRestart
			if why != nil {
				msg = why(fleetName, inst)
			}
			inst.Status = fleet.StatusFailed
			inst.Error = msg
			failed = append(failed, interruptedRecord{key: fleetName + "/" + inst.Name, err: msg})
		}
	}
	return failed
}
