//go:build !nok8s

package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_reconcile.go is what a daemon does with the Kubernetes jobs a
// previous one died in, and with its known garbage. The job journals
// (internal/backend/kubernetes/journal.go) say which jobs those were and what
// each had written. A job is never resumed:
//
//	create   its record goes FAILED kubernetes:interrupted at start, before
//	         any RPC, with no cluster I/O. Its objects stay, labelled with
//	         its key, until a retry (a rebuild from FAILED) deletes them by
//	         key and creates again, or a destroy deletes them by key.
//	clone    likewise for the destination; once the cluster answers, the
//	         copier Jobs and snapshots it made are deleted (the source is
//	         never touched).
//	rebuild  undone, in a job of its own that keeps the record REBUILDING
//	         until it is done, once the cluster answers: what the rebuild
//	         made is deleted and the replicas restored, and the record goes
//	         back to the status it had; one past its reset first has the
//	         rollback tool Job put the previous root back and the previous
//	         generation's StatefulSet re-applied, and goes FAILED
//	         kubernetes:rebuild_failed at that generation.
//
// An unreachable cluster is waited for: the undo, and the background pass,
// run when the connection is next judged usable (the REACHABLE edge). A
// journal the live objects no longer match (restored from an older backup
// after its job finished) is never acted on: it is discarded with a
// fleet.log line, and its record, like any other backend's, goes FAILED
// "interrupted by daemon restart".
//
// The background pass also deletes known garbage, which needs no `fleet
// cluster gc --apply`: the objects of a journal no record holds any more,
// and every object of a destroyed instance's key (its footprint tombstone)
// until none is left. All of it only while this daemon holds the
// connection's owner Lease, checked before each deletion.

// Error codes the reconciler leaves on a record.
const (
	codeInterrupted   = "kubernetes:interrupted"
	codeRebuildFailed = "kubernetes:rebuild_failed"
)

// Reconciler cadence; variables so tests can shorten them.
var (
	// kubeUndoPoll is how often an undo waiting for its cluster looks again
	// between REACHABLE edges.
	kubeUndoPoll = 5 * time.Second
	// kubeUndoAttempts and kubeUndoRetry bound the retries of an undo that
	// failed on a reachable cluster.
	kubeUndoAttempts = 3
	kubeUndoRetry    = 30 * time.Second
	// kubePassTimeout bounds one background pass of a connection;
	// kubePassFollowUp is when a pass that deleted something looks again.
	kubePassTimeout  = 5 * time.Minute
	kubePassFollowUp = 30 * time.Second
)

// kubeRecoveryPool is the part of the pool the reconciler drives
// (*kubernetes.Pool).
type kubeRecoveryPool interface {
	Scope(clusterID string) (kubernetes.Scope, error)
	UndoRebuild(ctx context.Context, j kubernetes.Journal, progress func(string)) (kubernetes.RebuildUndo, error)
	UndoClone(ctx context.Context, j kubernetes.Journal) (kubernetes.Teardown, error)
	SetOwnerGate(kubernetes.OwnerGate)
}

// kubeRecovery is the reconciler.
type kubeRecovery struct {
	svc  *service
	pool func() kubeRecoveryPool
	// jobsDir is the journals' directory (state.KubeJobsDir).
	jobsDir func() string

	wake chan struct{}

	mu sync.Mutex
	// journals are the journals read at start, by key.
	journals map[string]kubernetes.JournalFile
	// claimed are the interrupted rebuilds whose undo jobs start runs, by
	// "fleet/instance".
	claimed map[string]kubernetes.Journal
	// failedAt maps the records failed at start to the journal they had.
	failedAt map[string]kubernetes.Journal
	// wanted are the connections whose background pass is due.
	wanted map[string]bool
	// edge is closed (and replaced) each time a connection is judged usable
	// with its Lease held.
	edge chan struct{}
	// pending counts each connection's known garbage not yet gone.
	pending map[string]int
	looping bool
}

func newKubeRecovery(svc *service) *kubeRecovery {
	return &kubeRecovery{
		svc:      svc,
		pool:     func() kubeRecoveryPool { return kubernetes.DefaultPool() },
		jobsDir:  state.KubeJobsDir,
		wake:     make(chan struct{}, 1),
		journals: map[string]kubernetes.JournalFile{},
		claimed:  map[string]kubernetes.Journal{},
		failedAt: map[string]kubernetes.Journal{},
		wanted:   map[string]bool{},
		edge:     make(chan struct{}),
		pending:  map[string]int{},
	}
}

// load reads the journals a previous daemon left (local files only).
func (r *kubeRecovery) load() {
	files, err := kubernetes.ListJournals(r.jobsDir())
	if err != nil {
		flog.Warn("kube reconcile: reading the job journals", "err", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.journals = map[string]kubernetes.JournalFile{}
	for _, f := range files {
		if f.Err != nil {
			flog.Warn("kube reconcile: leaving a job journal alone", "path", f.Path, "err", f.Err)
			continue
		}
		r.journals[f.Journal.Key] = f
	}
}

// journalOf is the journal recorded for inst, if it names inst.
func (r *kubeRecovery) journalOfLocked(fleetName string, inst *fleet.Instance) (kubernetes.Journal, bool) {
	if inst.Backend != fleet.BackendKubernetes || inst.KubeKey == "" {
		return kubernetes.Journal{}, false
	}
	f, ok := r.journals[inst.KubeKey]
	if !ok {
		return kubernetes.Journal{}, false
	}
	j := f.Journal
	if j.Cluster != inst.KubeCluster || j.Fleet != fleetName || j.Instance != inst.Name {
		return kubernetes.Journal{}, false
	}
	return j, true
}

// claim picks the interrupted rebuilds the reconciler undoes itself: each
// Kubernetes record that is REBUILDING with its journal (or FAILED by an
// undo that failed before), left for the undo job start runs. Called under
// the state lock, by failInterruptedJobs, before records are failed.
func (r *kubeRecovery) claim(st *state.State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for fleetName, f := range st.Fleets {
		if f == nil {
			continue
		}
		for _, inst := range f.Instances {
			j, ok := r.journalOfLocked(fleetName, inst)
			if !ok || j.Op != kubernetes.OpRebuild {
				continue
			}
			if inst.Status == fleet.StatusRebuilding || (inst.Status == fleet.StatusFailed && strings.HasPrefix(inst.Error, codeRebuildFailed+": undoing")) {
				r.claimed[fleetName+"/"+inst.Name] = j
			}
		}
	}
}

// claims reports whether claim picked fleetName/instanceName.
func (r *kubeRecovery) claims(fleetName, instanceName string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.claimed[fleetName+"/"+instanceName]
	return ok
}

// interruptedError is the error a record failed at start carries: for a
// Kubernetes one, kubernetes:interrupted with the job and step its journal
// names.
func (r *kubeRecovery) interruptedError(fleetName string, inst *fleet.Instance) string {
	if inst.Backend != fleet.BackendKubernetes {
		return interruptedByRestart
	}
	op, _ := kubernetes.OpForStatus(inst.Status)
	r.mu.Lock()
	j, ok := r.journalOfLocked(fleetName, inst)
	if ok {
		r.failedAt[fleetName+"/"+inst.Name] = j
		op = j.Op
	}
	r.mu.Unlock()
	msg := fmt.Sprintf("%s: %s during %s", codeInterrupted, interruptedByRestart, op)
	if ok {
		msg += fmt.Sprintf(" (at step %s)", j.Step)
	}
	return msg + "; it was not resumed: " + interruptedRetry(inst.Backend)
}

// interruptedRetry is how to retry an interrupted job on backend t: a
// rebuild where the backend can rebuild, else destroying the instance and
// creating it again.
func interruptedRetry(t fleet.BackendType) string {
	if t.Capabilities().Rebuild {
		return "rebuild it to retry, or destroy it"
	}
	return "destroy it and create it again"
}

// settle finishes what start-up marking left of the failed records'
// journals: a create's journal has nothing left to do (its objects stay with
// the record); a clone's waits for the background pass to delete its copier
// Jobs and snapshots.
func (r *kubeRecovery) settle() {
	r.mu.Lock()
	failed := r.failedAt
	r.failedAt = map[string]kubernetes.Journal{}
	r.mu.Unlock()
	for key, j := range failed {
		switch j.Op {
		case kubernetes.OpCreate:
			if err := kubernetes.RemoveJournal(kubernetes.JournalPath(j.Key)); err != nil {
				flog.Warn("kube reconcile: removing a create's journal", "instance", key, "err", err)
			} else {
				flog.Info("kube reconcile: create interrupted by daemon restart; record failed, objects kept", "instance", key, "cluster", j.Cluster, "step", j.Step)
			}
		default:
			r.want(j.Cluster)
		}
	}
}

// start runs the undo jobs of the claimed rebuilds and the background
// passes, until ctx ends. The serve loop calls it once the hub runs, before
// fleetd serves.
func (r *kubeRecovery) start(ctx context.Context) {
	p := r.pool()
	p.SetOwnerGate(r.svc.kube.ownerGate)
	r.mu.Lock()
	claimed := r.claimed
	r.claimed = map[string]kubernetes.Journal{}
	loop := !r.looping
	r.looping = true
	r.mu.Unlock()
	for key, j := range claimed {
		fleetName, instanceName, _ := strings.Cut(key, "/")
		r.startUndo(fleetName, instanceName, j)
	}
	if loop {
		go r.loop(ctx)
	}
}

// reachable is told by a connection's worker each time a full preflight
// finds it usable with this daemon holding its owner Lease (the first one
// after start, or after the cluster was unreachable, is the REACHABLE
// edge). Undo jobs waiting on it go, and its background pass is due.
func (r *kubeRecovery) reachable(clusterID string) {
	r.mu.Lock()
	close(r.edge)
	r.edge = make(chan struct{})
	r.mu.Unlock()
	r.want(clusterID)
}

func (r *kubeRecovery) want(clusterID string) {
	r.mu.Lock()
	r.wanted[clusterID] = true
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// pendingTeardowns is the connection's known garbage not yet gone (its
// KubeCapabilities.pending_teardowns).
func (r *kubeRecovery) pendingTeardowns(clusterID string) int32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return int32(r.pending[clusterID])
}

func (r *kubeRecovery) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
		r.mu.Lock()
		var due []string
		for id := range r.wanted {
			due = append(due, id)
		}
		r.wanted = map[string]bool{}
		r.mu.Unlock()
		for _, id := range due {
			if ctx.Err() != nil {
				return
			}
			r.pass(ctx, id)
		}
	}
}

// usable reports whether connection id was judged usable, with this daemon
// holding its Lease, and is still.
func (r *kubeRecovery) usable(id string) bool {
	st := r.svc.kube.status(id)
	return preflight.Usable(st.GetState()) && r.svc.kube.leaseHeld(id)
}

// pass is a connection's background pass: the clones failed at start lose
// their copier Jobs and snapshots, the journals no record holds lose their
// objects, the journals of settled records are discarded, and the
// destroyed instances' keys lose whatever is left of them.
func (r *kubeRecovery) pass(ctx context.Context, clusterID string) {
	if _, ok := r.svc.kube.record(clusterID); !ok || !r.usable(clusterID) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, kubePassTimeout)
	defer cancel()
	scope, err := r.pool().Scope(clusterID)
	if err != nil {
		flog.Warn("kube reconcile: the connection does not load", "cluster", clusterID, "err", err)
		return
	}
	// The journals and tombstones are read before the records: a job's
	// record is saved before its journal is begun, and a destroy's DELETING
	// mark before its tombstone, so the records read next hold every record
	// that wrote what was read (one read before them could miss a job or a
	// destroy started meanwhile, and take its journal for garbage or its
	// tombstone for a key held again).
	files, err := kubernetes.ListJournals(r.jobsDir())
	if err != nil {
		flog.Warn("kube reconcile: reading the job journals", "err", err)
	}
	fp, err := kubernetes.LoadFootprint()
	if err != nil {
		flog.Warn("kube reconcile: reading the footprint", "err", err)
	}
	st, err := state.Load()
	if err != nil {
		flog.Warn("kube reconcile: load state", "err", err)
		return
	}
	pendingJournals, deletedJournals := r.passJournals(ctx, scope, st, files)
	pendingKeys, deletedKeys, waiting := r.passTombstones(ctx, scope, st, fp)
	pending := pendingJournals + pendingKeys
	r.mu.Lock()
	changed := r.pending[clusterID] != pending
	r.pending[clusterID] = pending
	r.mu.Unlock()
	if changed {
		r.svc.kube.publish()
	}
	if (pending > 0 && deletedJournals+deletedKeys > 0) || waiting {
		// Deletions under way (a foreground delete takes its time), or a
		// cancelled job still winding down: look again soon, not at the
		// next full preflight.
		time.AfterFunc(kubePassFollowUp, func() { r.want(clusterID) })
	}
}

// holder is the record that holds key on clusterID, if any.
func holder(st *state.State, clusterID, key string) (string, *fleet.Instance) {
	for name, f := range st.Fleets {
		if f == nil {
			continue
		}
		for _, inst := range f.Instances {
			if inst.KubeCluster == clusterID && inst.KubeKey == key {
				return name, inst
			}
		}
	}
	return "", nil
}

// passJournals handles the connection's journals (files, read before st) no
// job is writing, returning how many are known garbage still there and how
// many objects it deleted.
func (r *kubeRecovery) passJournals(ctx context.Context, scope kubernetes.Scope, st *state.State, files []kubernetes.JournalFile) (pending, deleted int) {
	for _, f := range files {
		j := f.Journal
		if f.Err != nil || j.Cluster != scope.Cluster {
			continue
		}
		if r.svc.jobs.hasProvisioningJob(j.Fleet, j.Instance) {
			continue // a job of this daemon's is writing it
		}
		fleetName, inst := holder(st, j.Cluster, j.Key)
		switch {
		case inst == nil:
			td, err := scope.DeleteGarbage(ctx, j)
			deleted += len(td.Deleted)
			var stale *kubernetes.StaleJournalError
			if errors.As(err, &stale) {
				flog.Info("kube reconcile: discarded a job journal no record holds that the cluster no longer matches; its objects, if any, are left to `fleet cluster gc`", "cluster", j.Cluster, "key", j.Key, "op", j.Op, "why", stale.Why)
				_ = kubernetes.RemoveJournal(f.Path)
				continue
			}
			if err != nil || len(td.Remaining) > 0 {
				pending++
				if err != nil {
					flog.Warn("kube reconcile: deleting a job's objects no record holds", "cluster", j.Cluster, "key", j.Key, "deleted", len(td.Deleted), "err", err)
				}
				continue
			}
			flog.Info("kube reconcile: deleted a job's objects no record holds", "cluster", j.Cluster, "key", j.Key, "op", j.Op, "fleet", j.Fleet, "instance", j.Instance, "deleted", len(td.Deleted))
			_ = kubernetes.RemoveJournal(f.Path)
		case inst.Status == fleet.StatusDeleting || isTransitionalStatus(inst.Status):
			// A destroy (or a job) is on it.
		case j.Op == kubernetes.OpClone && inst.Status == fleet.StatusFailed:
			td, err := r.pool().UndoClone(ctx, j)
			var stale *kubernetes.StaleJournalError
			if errors.As(err, &stale) {
				flog.Info("kube reconcile: discarded a clone journal the cluster no longer matches", "instance", fleetName+"/"+inst.Name, "why", stale.Why)
				_ = kubernetes.RemoveJournal(f.Path)
				continue
			}
			if err != nil {
				flog.Warn("kube reconcile: deleting an interrupted clone's copiers", "instance", fleetName+"/"+inst.Name, "err", err)
				continue
			}
			flog.Info("kube reconcile: deleted an interrupted clone's copiers and snapshots", "instance", fleetName+"/"+inst.Name, "deleted", len(td.Deleted))
			_ = kubernetes.RemoveJournal(f.Path)
		case j.Op == kubernetes.OpRebuild && inst.Status == fleet.StatusFailed && strings.HasPrefix(inst.Error, codeRebuildFailed+": undoing"):
			// An undo that failed: kept for the next start's.
		default:
			flog.Info("kube reconcile: discarded a job journal its record has outlived", "instance", fleetName+"/"+inst.Name, "op", j.Op, "step", j.Step, "status", inst.Status)
			_ = kubernetes.RemoveJournal(f.Path)
		}
	}
	return pending, deleted
}

// passTombstones deletes what is left of the connection's destroyed
// instances (fp, read before st), returning how many keys still have
// objects, how many objects it deleted, and whether a key with nothing left
// is kept for a cancelled job of its instance that still winds down.
func (r *kubeRecovery) passTombstones(ctx context.Context, scope kubernetes.Scope, st *state.State, fp *kubernetes.Footprint) (pending, deleted int, waiting bool) {
	if fp == nil {
		return 0, 0, false
	}
	for _, t := range fp.TombstonesOf(scope.Cluster) {
		if _, inst := holder(st, t.Cluster, t.Key); inst != nil && inst.Status != fleet.StatusDeleting {
			flog.Info("kube reconcile: a record holds a destroyed key again; forgetting its tombstone", "cluster", t.Cluster, "key", t.Key)
			_ = kubernetes.DropTombstoneOf(t)
			continue
		}
		left, err := scope.ListByKey(ctx, t.Key)
		if err != nil {
			pending++
			flog.Warn("kube reconcile: listing a destroyed instance's objects", "cluster", t.Cluster, "key", t.Key, "err", err)
			continue
		}
		if len(left) == 0 {
			if t.Fleet != "" && r.svc.jobs.hasProvisioningJob(t.Fleet, t.Instance) {
				// A cancelled job of the instance that has not ended yet
				// may still write an object: keep the key until it ends.
				waiting = true
				continue
			}
			_ = kubernetes.DropTombstoneOf(t)
			continue
		}
		pending++
		td, err := scope.DeleteByKey(ctx, t.Key)
		deleted += len(td.Deleted)
		if err != nil {
			flog.Warn("kube reconcile: deleting a destroyed instance's objects", "cluster", t.Cluster, "key", t.Key, "deleted", len(td.Deleted), "err", err)
			continue
		}
		if len(td.Deleted) > 0 {
			flog.Info("kube reconcile: deleted what was left of a destroyed instance", "cluster", t.Cluster, "key", t.Key, "fleet", t.Fleet, "instance", t.Instance, "deleted", len(td.Deleted))
		}
	}
	return pending, deleted, waiting
}

// destroying tombstones the key of a Kubernetes record a destroy is about
// to tear down: whatever of it the teardown does not delete (the cluster is
// unreachable, the daemon dies halfway, its backend leaves something) is
// known garbage the background pass deletes.
func (r *kubeRecovery) destroying(fleetName string, inst *fleet.Instance) {
	if inst == nil || inst.Backend != fleet.BackendKubernetes || inst.KubeCluster == "" || inst.KubeKey == "" {
		return
	}
	t := kubernetes.Tombstone{Cluster: inst.KubeCluster, Key: inst.KubeKey, Fleet: fleetName, Instance: inst.Name}
	if err := kubernetes.AddTombstone(t); err != nil {
		flog.Warn("kube reconcile: recording a destroyed instance's key", "cluster", t.Cluster, "key", t.Key, "err", err)
	}
}

// destroyed asks for the background pass of a destroyed Kubernetes
// record's connection, once the record is gone.
func (r *kubeRecovery) destroyed(inst *fleet.Instance) {
	if r == nil || inst == nil || inst.Backend != fleet.BackendKubernetes || inst.KubeCluster == "" || inst.KubeKey == "" {
		return
	}
	r.want(inst.KubeCluster)
}

// startUndo runs the undo of an interrupted rebuild as a rebuild job of its
// own: the record stays REBUILDING (so nothing else acts on it), a destroy
// cancels it, and its steps show on the row.
func (r *kubeRecovery) startUndo(fleetName, instanceName string, j kubernetes.Journal) {
	s := r.svc
	_ = state.Update(func(st *state.State) error {
		if inst := findInstance(st, fleetName, instanceName); inst != nil && inst.KubeKey == j.Key && inst.Status == fleet.StatusFailed {
			inst.Status = fleet.StatusRebuilding
		}
		return nil
	})
	job := s.jobs.start(s.bgCtx, fleetgrpc.JobKind_JOB_KIND_REBUILD_INSTANCE, fleetName, instanceName, time.Now())
	go s.runJob(job, func() (*fleetgrpc.Instance, []string, error) {
		plog := continueProvisioningLog(job, fleetName, instanceName, "rollback of the interrupted rebuild")
		err := r.undo(job.ctx, plog, fleetName, instanceName, j)
		plog.finish(err)
		return loadInstanceSnapshot(fleetName, instanceName), nil, err
	})
}

// undo waits for the rebuild's cluster, undoes the rebuild and records the
// outcome on the record.
func (r *kubeRecovery) undo(ctx context.Context, plog *provisioningLog, fleetName, instanceName string, j kubernetes.Journal) error {
	key := fleetName + "/" + instanceName
	progress := func(step string) { plog.Step(fleet.StepRollback, step) }
	var lastErr error
	for attempt := 1; ; attempt++ {
		if err := r.awaitUsable(ctx, j.Cluster, progress); errors.Is(err, errConnectionGone) {
			msg := fmt.Sprintf("%s: %s during rebuild, and its connection %s is gone", codeInterrupted, interruptedByRestart, j.Cluster)
			r.finishUndo(fleetName, instanceName, j, fleet.StatusFailed, msg, true)
			return errors.New(msg)
		} else if err != nil {
			return err
		}
		res, err := r.pool().UndoRebuild(ctx, j, progress)
		var stale *kubernetes.StaleJournalError
		switch {
		case errors.As(err, &stale):
			flog.Warn("kube reconcile: discarded a rebuild journal the cluster no longer matches", "instance", key, "why", stale.Why)
			r.finishUndo(fleetName, instanceName, j, fleet.StatusFailed, interruptedByRestart, true)
			return errors.New(interruptedByRestart)
		case err == nil && res.RolledBack:
			msg := fmt.Sprintf("%s: the rebuild to generation %d was interrupted by a daemon restart; generation %d was restored. Rebuild again to retry", codeRebuildFailed, j.Gen, j.Gen-1)
			flog.Info("kube reconcile: rolled an interrupted rebuild back", "instance", key, "gen", j.Gen-1, "deleted", len(res.Teardown.Deleted))
			r.finishUndo(fleetName, instanceName, j, fleet.StatusFailed, msg, true)
			return errors.New(msg)
		case err == nil:
			flog.Info("kube reconcile: undid an interrupted rebuild before its reset", "instance", key, "status", j.PrevStatus, "deleted", len(res.Teardown.Deleted))
			status := j.PrevStatus
			if status == "" {
				status = fleet.StatusStopped
			}
			r.finishUndo(fleetName, instanceName, j, status, j.PrevError, true)
			// The job is the rebuild's, which did not happen.
			return fmt.Errorf("the rebuild was interrupted by a daemon restart and undone; the instance is %s as before", status)
		case ctx.Err() != nil:
			return ctx.Err()
		}
		lastErr = err
		flog.Warn("kube reconcile: undoing an interrupted rebuild failed", "instance", key, "attempt", attempt, "err", err)
		if attempt >= kubeUndoAttempts {
			break
		}
		plog.Warn(fmt.Sprintf("undoing the rebuild failed (attempt %d of %d): %s", attempt, kubeUndoAttempts, preflight.Message(err)))
		if err := sleepUntil(ctx, kubeUndoRetry); err != nil {
			return err
		}
	}
	msg := fmt.Sprintf("%s: undoing the rebuild a daemon restart interrupted failed: %s; it is tried again when fleetd next starts", codeRebuildFailed, preflight.Message(lastErr))
	r.finishUndo(fleetName, instanceName, j, fleet.StatusFailed, msg, false)
	return errors.New(msg)
}

// errConnectionGone is an undo whose connection was removed.
var errConnectionGone = errors.New("the connection is gone")

// awaitUsable waits for the connection's next usable judgement with the
// Lease held (now, if it has one); errConnectionGone once the connection is
// removed.
func (r *kubeRecovery) awaitUsable(ctx context.Context, clusterID string, progress func(string)) error {
	said := false
	for {
		r.mu.Lock()
		edge := r.edge
		r.mu.Unlock()
		if _, ok := r.svc.kube.record(clusterID); !ok {
			return errConnectionGone
		}
		if r.usable(clusterID) {
			return nil
		}
		if !said {
			progress("waiting for cluster connection " + clusterID)
			said = true
		}
		timer := time.NewTimer(kubeUndoPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-edge:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// finishUndo records an undo's outcome on the record, if the record is
// still the one undone (not destroyed meanwhile), and removes the journal
// when done.
func (r *kubeRecovery) finishUndo(fleetName, instanceName string, j kubernetes.Journal, status fleet.InstanceStatus, errMsg string, done bool) {
	_ = state.Update(func(st *state.State) error {
		inst := findInstance(st, fleetName, instanceName)
		if inst == nil || inst.KubeKey != j.Key || inst.Status != fleet.StatusRebuilding {
			return nil
		}
		inst.Status, inst.Error = status, errMsg
		return nil
	})
	if done {
		if err := kubernetes.RemoveJournal(kubernetes.JournalPath(j.Key)); err != nil {
			flog.Warn("kube reconcile: removing a rebuild's journal", "instance", fleetName+"/"+instanceName, "err", err)
		}
	}
	r.svc.pushState()
}

func sleepUntil(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ownerGate is the pool's owner check (kubernetes.OwnerGate): this daemon
// holds connection id's owner Lease, as its worker last renewed it, or a
// claim now says so.
func (m *kubeManager) ownerGate(ctx context.Context, id string) error {
	if m.leaseHeld(id) {
		return nil
	}
	w := m.worker(id)
	if w == nil {
		if _, ok := m.record(id); !ok {
			return fmt.Errorf("no Kubernetes connection %q", id)
		}
		return fmt.Errorf("%w: connection %s has not been checked yet", kubernetes.ErrOwnerConflict, id)
	}
	_, lease, _, err := w.ensureLink(false)
	if err != nil {
		_, msg := preflight.Classify(err)
		return fmt.Errorf("the connection does not load: %s", msg)
	}
	ctx, cancel := context.WithTimeout(ctx, kubeOwnerClaimTimeout)
	defer cancel()
	lst, err := lease.Ensure(ctx)
	if err != nil {
		_, msg := preflight.Classify(err)
		return fmt.Errorf("claiming the owner Lease: %s", msg)
	}
	if !lst.Held {
		return fmt.Errorf("%w: %s", kubernetes.ErrOwnerConflict, preflight.OwnerConflictMessage(lst, m.now()))
	}
	return nil
}

// kubeOwnerClaimTimeout bounds the Lease claim of an owner check.
const kubeOwnerClaimTimeout = 15 * time.Second
