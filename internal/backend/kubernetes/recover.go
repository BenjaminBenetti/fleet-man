//go:build !nok8s

package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// recover.go is the cluster side of the reconciler's table
// (internal/server/kube_reconcile.go) for a job a daemon died in: it is
// never resumed. A clone's copier Jobs and snapshots are deleted (the source
// is not touched; the destination's record fails). A rebuild is undone: what
// it made for the next generation is deleted and the StatefulSet's replicas
// restored, and one that got past its reset first has the rollback tool Job
// put the previous root back and the previous generation's StatefulSet
// re-applied. A journal the live objects no longer match (restored from an
// older backup after its job finished) is never acted on.

// StaleJournalError is a journal the live objects no longer match.
type StaleJournalError struct{ Why string }

func (e *StaleJournalError) Error() string {
	return "the job journal no longer matches the cluster: " + e.Why
}

// RebuildUndo is what undoing a rebuild did.
type RebuildUndo struct {
	// RolledBack: the rebuild was past its reset, and the rollback tool Job
	// put generation Gen-1's root back.
	RolledBack bool
	// Teardown are the objects the rebuild made that were deleted.
	Teardown Teardown
}

// UndoRebuild undoes the interrupted rebuild j records, on its connection,
// under the pool's owner check. A journal the cluster no longer matches is a
// *StaleJournalError and nothing is touched. progress, when set, hears each
// step.
func (p *Pool) UndoRebuild(ctx context.Context, j Journal, progress func(string)) (RebuildUndo, error) {
	say := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	if j.Op != OpRebuild {
		return RebuildUndo{}, fmt.Errorf("not a rebuild journal (%s)", j.Op)
	}
	s, err := p.Scope(j.Cluster)
	if err != nil {
		return RebuildUndo{}, err
	}
	if err := s.gate(ctx); err != nil {
		return RebuildUndo{}, err
	}
	check, err := s.CheckJournal(ctx, j)
	if err != nil {
		return RebuildUndo{}, err
	}
	if !check.Current {
		return RebuildUndo{}, &StaleJournalError{Why: check.Why}
	}
	anchor, ok := j.Anchor()
	if !ok || anchor.Kind != KindStatefulSet {
		return RebuildUndo{}, fmt.Errorf("the rebuild journal names no StatefulSet")
	}
	var undo RebuildUndo
	if j.ResetStarted {
		if j.Rollback == nil || len(j.Rollback.Job) == 0 || len(j.Rollback.StatefulSet) == 0 {
			return undo, fmt.Errorf("the rebuild was past its reset, and its journal holds no rollback: generation %d's root cannot be put back", j.Gen-1)
		}
		say("stopping the instance")
		if err := p.Scale(ctx, j.Cluster, anchor.Name, 0); err != nil {
			return undo, err
		}
		if err := p.waitPodGone(ctx, s, names.Pod(anchor.Name)); err != nil {
			return undo, err
		}
		say(fmt.Sprintf("restoring generation %d's root", j.Gen-1))
		if err := p.runRollbackJob(ctx, s, j); err != nil {
			return undo, err
		}
		undo.RolledBack = true
		say(fmt.Sprintf("re-applying generation %d's StatefulSet", j.Gen-1))
		if err := s.applyStatefulSet(ctx, j.Rollback.StatefulSet, j.PrevReplicas); err != nil {
			return undo, err
		}
		// The rollback Job is the reconciler's own, not the journal's:
		// read it back so it goes with the rest.
		if check, err = s.CheckJournal(ctx, withRollbackJob(j)); err != nil {
			return undo, err
		}
	}
	say(fmt.Sprintf("deleting generation %d's objects", j.Gen))
	undo.Teardown, err = s.DeleteJournalObjects(ctx, withRollbackJob(j), check, func(o JournalObject) bool { return o.Created })
	if err != nil {
		return undo, err
	}
	if j.PrevReplicas != nil {
		say("restoring the StatefulSet's replicas")
		if err := p.Scale(ctx, j.Cluster, anchor.Name, *j.PrevReplicas); err != nil {
			return undo, err
		}
	}
	if err := s.ClearMark(ctx, j); err != nil {
		s.logger().Warn("kube: clearing the rebuild's mark", "cluster", j.Cluster, "err", err)
	}
	return undo, nil
}

// rollbackJobName is the rollback tool Job's name in j's Rollback.
func rollbackJobName(j Journal) string {
	if j.Rollback == nil {
		return ""
	}
	var meta struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if json.Unmarshal(j.Rollback.Job, &meta) != nil {
		return ""
	}
	return meta.Metadata.Name
}

// withRollbackJob is j with its rollback Job among the objects it made.
func withRollbackJob(j Journal) Journal {
	name := rollbackJobName(j)
	if name == "" {
		return j
	}
	for _, o := range j.Objects {
		if o.Kind == KindJob && o.Name == name {
			return j
		}
	}
	j = j.clone()
	j.Objects = append(j.Objects, JournalObject{Kind: KindJob, Name: name, Gen: j.Gen - 1, Created: true})
	return j
}

// runRollbackJob runs the rollback tool Job j recorded: generation Gen-1's
// root comes back from trash (podseed's OpRollback, idempotent, so a
// rollback the daemon died in is finished by running it again).
func (p *Pool) runRollbackJob(ctx context.Context, s Scope, j Journal) error {
	name := rollbackJobName(j)
	if name == "" {
		return fmt.Errorf("the journal's rollback Job has no name")
	}
	// A Job from an earlier attempt has run (or is running) to its end: a
	// fresh one runs the rollback again.
	if existing, err := s.Client.BatchV1().Jobs(s.Namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		if err := s.gate(ctx); err != nil {
			return err
		}
		propagation := metav1.DeletePropagationForeground
		opts := metav1.DeleteOptions{PropagationPolicy: &propagation, Preconditions: &metav1.Preconditions{UID: &existing.UID}}
		if err := s.Client.BatchV1().Jobs(s.Namespace).Delete(ctx, name, opts); err != nil && !gone(err) {
			return fmt.Errorf("deleting the earlier rollback Job %s: %w", name, err)
		}
		if err := p.waitJobGone(ctx, s, name); err != nil {
			return err
		}
	}
	job, err := applyJob(ctx, s.Client, s.Namespace, j.Rollback.Job)
	if err != nil {
		return err
	}
	res, err := p.RunJob(ctx, JobRun{
		Cluster: j.Cluster,
		Job:     job.Name,
		Spec:    podseed.JobSpec{V: podseed.JobSpecVersion, Op: podseed.OpRollback, Gen: j.Gen - 1, PVC: podspec.JobDiskPath},
	})
	if err != nil {
		return fmt.Errorf("rollback Job %s: %w", name, err)
	}
	if !res.OK {
		return fmt.Errorf("rollback Job %s failed: %s", name, res.Error)
	}
	return nil
}

// applyStatefulSet re-applies a StatefulSet's apply body, with its replicas
// set to replicas when given (so a stopped instance is not started by its
// own rollback).
func (s Scope) applyStatefulSet(ctx context.Context, body []byte, replicas *int32) error {
	if replicas != nil {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return fmt.Errorf("the journal's StatefulSet: %w", err)
		}
		spec, _ := m["spec"].(map[string]any)
		if spec == nil {
			spec = map[string]any{}
			m["spec"] = spec
		}
		spec["replicas"] = json.Number(strconv.Itoa(int(*replicas)))
		var err error
		if body, err = json.Marshal(m); err != nil {
			return err
		}
	}
	var meta struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &meta); err != nil || meta.Metadata.Name == "" {
		return fmt.Errorf("the journal's StatefulSet has no name")
	}
	force := true
	_, err := s.Client.AppsV1().StatefulSets(s.Namespace).Patch(ctx, meta.Metadata.Name, types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: DefaultFieldManager, Force: &force})
	if err != nil {
		return fmt.Errorf("re-applying StatefulSet %s: %w", meta.Metadata.Name, err)
	}
	return nil
}

// UndoClone deletes the copier Jobs and snapshots of the interrupted clone j
// records (its source is not touched; the destination's other objects stay
// with its failed record, to be retried or destroyed by key).
func (p *Pool) UndoClone(ctx context.Context, j Journal) (Teardown, error) {
	if j.Op != OpClone {
		return Teardown{}, fmt.Errorf("not a clone journal (%s)", j.Op)
	}
	s, err := p.Scope(j.Cluster)
	if err != nil {
		return Teardown{}, err
	}
	return undoClone(ctx, s, j)
}

func undoClone(ctx context.Context, s Scope, j Journal) (Teardown, error) {
	if err := s.gate(ctx); err != nil {
		return Teardown{}, err
	}
	check, err := s.CheckJournal(ctx, j)
	if err != nil {
		return Teardown{}, err
	}
	t, err := s.DeleteJournalObjects(ctx, j, check, func(o JournalObject) bool {
		return o.Created && (o.Kind == KindJob || o.Kind == KindVolumeSnapshot)
	})
	if err != nil {
		return t, err
	}
	if err := s.ClearMark(ctx, j); err != nil {
		s.logger().Warn("kube: clearing the clone's mark", "cluster", j.Cluster, "err", err)
	}
	return t, nil
}

// DeleteGarbage deletes what j recorded when no record holds its key any
// more: every object of the journal still there under the UID it recorded
// (or, unrecorded, carrying the key). Known garbage, deleted without `fleet
// cluster gc`, but only while the journal is current: a job that died
// mid-way. A journal the cluster no longer matches (its job finished, as for
// one a backup restored) is a *StaleJournalError and nothing is deleted;
// what it names is no more known garbage than any other unaccounted object.
func (s Scope) DeleteGarbage(ctx context.Context, j Journal) (Teardown, error) {
	if err := s.gate(ctx); err != nil {
		return Teardown{}, err
	}
	check, err := s.CheckJournal(ctx, j)
	if err != nil {
		return Teardown{}, err
	}
	if !check.Current {
		return Teardown{}, &StaleJournalError{Why: check.Why}
	}
	return s.DeleteJournalObjects(ctx, j, check, nil)
}

// waitPodGone waits for an instance pod to be gone after a scale to 0.
func (p *Pool) waitPodGone(ctx context.Context, s Scope, pod string) error {
	d := DefaultDeadlines()
	deadline := p.opts.Now().Add(d.PodReady)
	for {
		_, err := s.Client.CoreV1().Pods(s.Namespace).Get(ctx, pod, metav1.GetOptions{})
		if gone(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading pod %s: %w", pod, err)
		}
		if p.opts.Now().After(deadline) {
			return &DeadlineError{Kind: CodeTimeout, Wait: "pod " + pod + " to stop", After: d.PodReady, Env: EnvPodReadyTimeout}
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			return err
		}
	}
}

// waitJobGone waits for a deleted Job to be gone.
func (p *Pool) waitJobGone(ctx context.Context, s Scope, name string) error {
	deadline := p.opts.Now().Add(DefaultDeadlines().PodReady)
	for {
		_, err := s.Client.BatchV1().Jobs(s.Namespace).Get(ctx, name, metav1.GetOptions{})
		if gone(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading Job %s: %w", name, err)
		}
		if p.opts.Now().After(deadline) {
			return &DeadlineError{Kind: CodeTimeout, Wait: "Job " + name + " to be deleted", After: DefaultDeadlines().PodReady, Env: EnvPodReadyTimeout}
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			return err
		}
	}
}
