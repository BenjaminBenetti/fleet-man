//go:build !nok8s && linux

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

const (
	rbKey = "aaaaaaaa"
	rbSTS = "fi-a-one-aaaaaaaa"
)

// instMeta is the metadata of an object of the test instance on the
// harness's connection, marked with mark ("" none).
func instMeta(name, uid, mark string, role names.Role) metav1.ObjectMeta {
	labels := names.Base(testOwner, testCluster)
	labels[names.LabelKey] = rbKey
	if role != "" {
		labels[names.LabelRole] = string(role)
	}
	m := metav1.ObjectMeta{Name: name, Namespace: jobNS, Labels: labels, UID: types.UID(uid)}
	if mark != "" {
		m.Annotations = map[string]string{names.AnnotationOp: mark}
	}
	return m
}

func (h *jobHarness) seed(objs ...runtime.Object) {
	h.t.Helper()
	for _, o := range objs {
		if err := h.cs.Tracker().Add(o); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *jobHarness) exists(kind, name string) bool {
	h.t.Helper()
	k, _ := kindOf(kind)
	_, err := k.get(context.Background(), h.cs, jobNS, name)
	if err != nil && !gone(err) {
		h.t.Fatal(err)
	}
	return err == nil
}

func (h *jobHarness) sts() *appsv1.StatefulSet {
	h.t.Helper()
	s, err := h.cs.AppsV1().StatefulSets(jobNS).Get(context.Background(), rbSTS, metav1.GetOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// interruptedRebuild is the cluster and journal of a rebuild from gen 2 to
// gen 3 of a running instance, interrupted at step (past its reset when
// reset).
func (h *jobHarness) interruptedRebuild(step Step, reset bool) Journal {
	h.t.Helper()
	zero, one := int32(0), int32(1)
	h.seed(
		&appsv1.StatefulSet{ObjectMeta: instMeta(rbSTS, "u-sts", MarkFor(OpRebuild, step), names.RoleInstance), Spec: appsv1.StatefulSetSpec{Replicas: &zero}},
		&corev1.PersistentVolumeClaim{ObjectMeta: instMeta(rbSTS, "u-pvc", "", "")},
		&corev1.Secret{ObjectMeta: instMeta(rbSTS+"-spec-g2", "u-spec2", "", "")},
		&corev1.Secret{ObjectMeta: instMeta(rbSTS+"-spec-g3", "u-spec3", MarkFor(OpRebuild, "render"), "")},
		&corev1.Secret{ObjectMeta: instMeta(rbSTS+"-env-g3", "u-env3", MarkFor(OpRebuild, "render"), "")},
		&batchv1.Job{ObjectMeta: instMeta(rbSTS+"-seed-g3", "u-seed3", MarkFor(OpRebuild, StepReset), names.RoleSeed)},
	)
	j := Journal{
		V: JournalVersion, Op: OpRebuild, Fleet: "alpha", Instance: "one", Cluster: testCluster, Namespace: jobNS, Key: rbKey, Gen: 3,
		Step: step, ResetStarted: reset, PrevReplicas: &one, PrevSpecSecret: rbSTS + "-spec-g2", PrevStatus: fleet.StatusRunning,
		Objects: []JournalObject{
			{Kind: KindStatefulSet, Name: rbSTS, UID: "u-sts", Anchor: true},
			{Kind: KindSecret, Name: rbSTS + "-spec-g3", UID: "u-spec3", Gen: 3, Created: true},
			{Kind: KindSecret, Name: rbSTS + "-env-g3", UID: "u-env3", Gen: 3, Created: true},
			{Kind: KindJob, Name: rbSTS + "-seed-g3", UID: "u-seed3", Gen: 3, Created: true},
		},
	}
	if reset {
		jobBody, _ := json.Marshal(map[string]any{
			"apiVersion": "batch/v1", "kind": "Job",
			"metadata": map[string]any{"name": rbSTS + "-rollback-g2", "labels": instMeta("", "", "", names.RoleTool).Labels},
			"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{"restartPolicy": "Never", "containers": []any{map[string]any{"name": podspec.JobContainer, "image": "img@sha256:aa"}}}}},
		})
		stsBody, _ := json.Marshal(map[string]any{
			"apiVersion": "apps/v1", "kind": "StatefulSet",
			"metadata": map[string]any{"name": rbSTS, "labels": instMeta("", "", "", names.RoleInstance).Labels, "annotations": map[string]any{names.AnnotationRebuildGen: "2"}},
			"spec":     map[string]any{"replicas": 1, "serviceName": "x", "template": map[string]any{"spec": map[string]any{"volumes": []any{map[string]any{"name": "spec", "secret": map[string]any{"secretName": rbSTS + "-spec-g2"}}}}}},
		})
		j.Rollback = &Rollback{Job: jobBody, StatefulSet: stsBody}
	}
	return j
}

// A rebuild interrupted before its reset is undone without a tool Job: what
// it made goes, the replicas come back, the mark is cleared, and the
// previous generation's spec stays.
func TestUndoRebuildBeforeReset(t *testing.T) {
	h := newJobHarness(t)
	j := h.interruptedRebuild("seed", false)
	var steps []string
	undo, err := h.pool.UndoRebuild(context.Background(), j, func(s string) { steps = append(steps, s) })
	if err != nil {
		t.Fatal(err)
	}
	if undo.RolledBack || len(undo.Teardown.Deleted) != 3 {
		t.Fatalf("undo %+v", undo)
	}
	for _, name := range []string{rbSTS + "-spec-g3", rbSTS + "-env-g3"} {
		if h.exists(KindSecret, name) {
			t.Errorf("%s is left", name)
		}
	}
	if h.exists(KindJob, rbSTS+"-seed-g3") || !h.exists(KindSecret, rbSTS+"-spec-g2") || !h.exists(KindPVC, rbSTS) {
		t.Error("the wrong objects went")
	}
	sts := h.sts()
	if *sts.Spec.Replicas != 1 || sts.Annotations[names.AnnotationOp] != "" {
		t.Fatalf("StatefulSet after the undo: replicas %d, annotations %v", *sts.Spec.Replicas, sts.Annotations)
	}
	if got := h.execKinds(); got != "" {
		t.Fatalf("an undo before the reset ran a Job: %s", got)
	}
	if strings.Join(steps, "; ") != "deleting generation 3's objects; restoring the StatefulSet's replicas" {
		t.Fatalf("steps %q", steps)
	}
}

// A rebuild past its reset gets the rollback tool Job (podseed's rollback of
// generation 2), then generation 2's StatefulSet back with the instance's
// replicas, and the Jobs and generation 3's objects go.
func TestUndoRebuildAfterReset(t *testing.T) {
	h := newJobHarness(t)
	j := h.interruptedRebuild("seed", true)
	zero := int32(0)
	j.PrevReplicas, j.PrevStatus = &zero, fleet.StatusStopped
	undo, err := h.pool.UndoRebuild(context.Background(), j, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !undo.RolledBack {
		t.Fatal("not rolled back")
	}
	if got := h.execKinds(); got != "probe,stage,stage,feed" {
		t.Fatalf("the rollback Job's execs: %s", got)
	}
	h.mu.Lock()
	spec := h.specs[0]
	h.mu.Unlock()
	if spec.Op != podseed.OpRollback || spec.Gen != 2 || spec.PVC != podspec.JobDiskPath {
		t.Fatalf("rollback spec %+v", spec)
	}
	sts := h.sts()
	if *sts.Spec.Replicas != 0 || sts.Annotations[names.AnnotationRebuildGen] != "2" || sts.Annotations[names.AnnotationOp] != "" {
		t.Fatalf("re-applied StatefulSet: replicas %d, annotations %v", *sts.Spec.Replicas, sts.Annotations)
	}
	if sts.Spec.Template.Spec.Volumes[0].Secret.SecretName != rbSTS+"-spec-g2" {
		t.Fatalf("not generation 2's template: %+v", sts.Spec.Template.Spec.Volumes)
	}
	for _, o := range []struct{ kind, name string }{{KindJob, rbSTS + "-rollback-g2"}, {KindJob, rbSTS + "-seed-g3"}, {KindSecret, rbSTS + "-spec-g3"}} {
		if h.exists(o.kind, o.name) {
			t.Errorf("%s/%s is left", o.kind, o.name)
		}
	}
	if !h.exists(KindSecret, rbSTS+"-spec-g2") {
		t.Error("generation 2's spec went")
	}
}

// A rollback that fails leaves generation 3's objects for the next try.
func TestUndoRebuildWhoseRollbackFails(t *testing.T) {
	h := newJobHarness(t)
	h.result.OK, h.result.Error = false, "no space left on device"
	j := h.interruptedRebuild("seed", true)
	if _, err := h.pool.UndoRebuild(context.Background(), j, nil); err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("err = %v", err)
	}
	if !h.exists(KindSecret, rbSTS+"-spec-g3") || h.sts().Annotations[names.AnnotationOp] == "" {
		t.Fatal("a failed rollback deleted the rebuild's objects or its mark")
	}
	// It runs again with a fresh Job.
	h.mu.Lock()
	h.result = jobboot.Result{OK: true}
	h.mu.Unlock()
	if undo, err := h.pool.UndoRebuild(context.Background(), j, nil); err != nil || !undo.RolledBack {
		t.Fatalf("second try: %+v %v", undo, err)
	}
}

// Past its reset with no rollback recorded, a rebuild cannot be undone, and
// nothing is touched.
func TestUndoRebuildWithoutARollback(t *testing.T) {
	h := newJobHarness(t)
	j := h.interruptedRebuild("seed", true)
	j.Rollback = nil
	if _, err := h.pool.UndoRebuild(context.Background(), j, nil); err == nil || !strings.Contains(err.Error(), "holds no rollback") {
		t.Fatalf("err = %v", err)
	}
	if !h.exists(KindSecret, rbSTS+"-spec-g3") {
		t.Fatal("touched")
	}
}

// A journal restored from a backup after its rebuild finished (the mark is
// gone) rolls nothing back.
func TestUndoRebuildOfAStaleJournal(t *testing.T) {
	h := newJobHarness(t)
	h.podFor = func(*batchv1.Job) *corev1.Pod { return nil }
	j := h.interruptedRebuild("seed", true)
	if _, err := h.cs.AppsV1().StatefulSets(jobNS).Patch(context.Background(), rbSTS, types.MergePatchType, annotationPatch(names.AnnotationOp, nil), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	before := len(h.cs.Actions())
	_, err := h.pool.UndoRebuild(context.Background(), j, nil)
	var stale *StaleJournalError
	if !errors.As(err, &stale) || !strings.Contains(stale.Why, "the job finished") {
		t.Fatalf("err = %v", err)
	}
	for _, a := range h.cs.Actions()[before:] {
		if v := a.GetVerb(); v != "get" && v != "list" && v != "watch" {
			t.Fatalf("a stale journal acted: %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

// An interrupted clone loses its copier Jobs, nothing else.
func TestUndoClone(t *testing.T) {
	h := newJobHarness(t)
	h.seed(
		&corev1.PersistentVolumeClaim{ObjectMeta: instMeta(rbSTS, "u-pvc", MarkFor(OpClone, "copy"), "")},
		&batchv1.Job{ObjectMeta: instMeta(rbSTS+"-copier-g1", "u-copier", MarkFor(OpClone, "copy"), names.RoleCopier)},
		&corev1.Secret{ObjectMeta: instMeta(rbSTS+"-spec-g1", "u-spec", "", "")},
	)
	j := Journal{V: JournalVersion, Op: OpClone, Fleet: "alpha", Instance: "one", Cluster: testCluster, Key: rbKey, Gen: 1, Step: "copy", Objects: []JournalObject{
		{Kind: KindPVC, Name: rbSTS, UID: "u-pvc", Created: true, Anchor: true},
		{Kind: KindSecret, Name: rbSTS + "-spec-g1", UID: "u-spec", Created: true},
		{Kind: KindJob, Name: rbSTS + "-copier-g1", UID: "u-copier", Created: true},
		{Kind: KindVolumeSnapshot, Name: "fi-src-two-bbbbbbbb-clone", Created: true},
	}}
	td, err := h.pool.UndoClone(context.Background(), j)
	if err != nil || strings.Join(td.Deleted, ",") != "job/"+rbSTS+"-copier-g1" {
		t.Fatalf("undo clone: %+v %v", td, err)
	}
	if !h.exists(KindPVC, rbSTS) || !h.exists(KindSecret, rbSTS+"-spec-g1") {
		t.Fatal("the destination's objects went")
	}
}

// Known garbage is deleted only as the journal knows it, and only while
// the journal is current: one the cluster no longer matches (an object made
// again since, the job's mark gone) deletes nothing.
func TestDeleteGarbage(t *testing.T) {
	journal := func(secretUID string) Journal {
		return Journal{V: JournalVersion, Op: OpCreate, Fleet: "alpha", Instance: "one", Cluster: testCluster, Key: rbKey, Gen: 1, Step: "seed", Objects: []JournalObject{
			{Kind: KindPVC, Name: rbSTS, UID: "u-pvc", Created: true, Anchor: true},
			{Kind: KindSecret, Name: rbSTS + "-spec-g1", UID: secretUID, Created: true},
			{Kind: KindJob, Name: rbSTS + "-seed-g1", Created: true},
		}}
	}
	for _, tc := range []struct {
		name, secretUID, mark string
		deleted               string
	}{
		{"current", "u-spec", MarkFor(OpCreate, "seed"), "job/" + rbSTS + "-seed-g1,pvc/" + rbSTS + ",secret/" + rbSTS + "-spec-g1"},
		{"an object made again", "u-old", MarkFor(OpCreate, "seed"), ""},
		{"the job finished", "u-spec", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newJobHarness(t)
			h.podFor = func(*batchv1.Job) *corev1.Pod { return nil }
			h.seed(
				&corev1.PersistentVolumeClaim{ObjectMeta: instMeta(rbSTS, "u-pvc", tc.mark, "")},
				&corev1.Secret{ObjectMeta: instMeta(rbSTS+"-spec-g1", "u-spec", "", "")},
				&batchv1.Job{ObjectMeta: instMeta(rbSTS+"-seed-g1", "u-seed", "", names.RoleSeed)},
				// The key's, but not the journal's: never a journal's to delete.
				&corev1.Secret{ObjectMeta: instMeta(rbSTS+"-env-g1", "u-env", "", "")},
			)
			s, err := h.pool.Scope(testCluster)
			if err != nil {
				t.Fatal(err)
			}
			td, err := s.DeleteGarbage(context.Background(), journal(tc.secretUID))
			var stale *StaleJournalError
			if tc.deleted == "" {
				if !errors.As(err, &stale) || len(td.Deleted) != 0 {
					t.Fatalf("stale garbage: %+v %v", td, err)
				}
				if !h.exists(KindPVC, rbSTS) {
					t.Fatal("deleted under a stale journal")
				}
				return
			}
			got := append([]string(nil), td.Deleted...)
			sort.Strings(got)
			if err != nil || strings.Join(got, ",") != tc.deleted {
				t.Fatalf("garbage: %v %v", got, err)
			}
			if !h.exists(KindSecret, rbSTS+"-env-g1") {
				t.Fatal("deleted an object the journal does not name")
			}
		})
	}
}

// A destroy's teardown by key records the tombstone before anything else,
// so a refused or failed deletion leaves known garbage; it removes the
// key's journal only when it is this connection's.
func TestDownByKey(t *testing.T) {
	h := newJobHarness(t)
	useFootprintAt(t)
	t.Setenv("HOME", t.TempDir())
	h.seed(
		&corev1.PersistentVolumeClaim{ObjectMeta: instMeta(rbSTS, "u-pvc", "", "")},
		&batchv1.Job{ObjectMeta: instMeta(rbSTS+"-seed-g1", "u-seed", "", names.RoleSeed)},
	)
	jj, err := BeginJournal(Journal{Op: OpCreate, Fleet: "alpha", Instance: "one", Cluster: testCluster, Key: rbKey, Gen: 1, Step: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	ts := Tombstone{Cluster: testCluster, Key: rbKey, Fleet: "alpha", Instance: "one"}

	h.pool.SetOwnerGate(func(context.Context, string) error { return ErrOwnerConflict })
	if _, err := h.pool.DownByKey(context.Background(), ts); !errors.Is(err, ErrOwnerConflict) {
		t.Fatalf("err = %v", err)
	}
	fp, _ := LoadFootprint()
	if len(fp.TombstonesOf(testCluster)) != 1 || !h.exists(KindPVC, rbSTS) {
		t.Fatalf("refused teardown: tombstones %+v", fp.Tombstones)
	}
	if _, err := os.Stat(jj.Path()); err != nil {
		t.Fatal("a refused teardown removed the journal")
	}

	h.pool.SetOwnerGate(nil)
	td, err := h.pool.DownByKey(context.Background(), ts)
	if err != nil || len(td.Deleted) != 2 || h.exists(KindPVC, rbSTS) {
		t.Fatalf("teardown %+v %v", td, err)
	}
	if _, err := os.Stat(jj.Path()); !os.IsNotExist(err) {
		t.Fatal("the journal is left")
	}
	if fp, _ := LoadFootprint(); len(fp.TombstonesOf(testCluster)) != 1 {
		t.Fatal("the tombstone went before the reconciler saw nothing left")
	}

	// Another connection's journal of the same key is not this teardown's.
	other := filepath.Join(filepath.Dir(jj.Path()), rbKey+".json")
	j := Journal{V: JournalVersion, Op: OpCreate, Fleet: "alpha", Instance: "one", Cluster: "other-conn", Key: rbKey, Gen: 1, Step: "seed", StartedAt: time.Now()}
	data, _ := json.Marshal(j)
	if err := os.WriteFile(other, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.DeleteByKey(context.Background(), testCluster, rbKey); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("removed another connection's journal")
	}
}

// WaitReady returns the pod once it is Ready, and names what it is stuck
// on when a deadline passes.
func TestWaitReady(t *testing.T) {
	h := newJobHarness(t)
	ctx := context.Background()
	h.seed(&corev1.PersistentVolumeClaim{ObjectMeta: instMeta(rbSTS, "u-pvc", "", ""), Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}})
	d := DefaultDeadlines()
	d.PVCBind = 50 * time.Millisecond
	_, err := h.pool.WaitReady(ctx, testCluster, rbSTS, WaitOptions{PVC: rbSTS, Interval: 5 * time.Millisecond, Deadlines: &d})
	var de *DeadlineError
	if !errors.As(err, &de) || de.Code() != CodePVCPending {
		t.Fatalf("err = %v", err)
	}

	pvc, _ := h.cs.CoreV1().PersistentVolumeClaims(jobNS).Get(ctx, rbSTS, metav1.GetOptions{})
	pvc.Status.Phase = corev1.ClaimBound
	_, _ = h.cs.CoreV1().PersistentVolumeClaims(jobNS).UpdateStatus(ctx, pvc, metav1.UpdateOptions{})
	go func() {
		time.Sleep(30 * time.Millisecond)
		pod := &corev1.Pod{ObjectMeta: instMeta(names.Pod(rbSTS), "u-pod", "", names.RoleInstance), Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
		_, _ = h.cs.CoreV1().Pods(jobNS).Create(ctx, pod, metav1.CreateOptions{})
	}()
	pod, err := h.pool.WaitReady(ctx, testCluster, rbSTS, WaitOptions{PVC: rbSTS, Interval: 5 * time.Millisecond})
	if err != nil || pod.UID != "u-pod" {
		t.Fatalf("ready: %v %v", pod, err)
	}

	stop := errors.New("FailedCreate: admission denied")
	_, err = h.pool.WaitReady(ctx, testCluster, "fi-a-none-cccccccc", WaitOptions{Interval: 5 * time.Millisecond, Check: func(context.Context) error { return stop }})
	if !errors.Is(err, stop) {
		t.Fatalf("the caller's check: %v", err)
	}
}
