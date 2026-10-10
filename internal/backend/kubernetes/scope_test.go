//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

const (
	scopeOwner   = "scopeowner12"
	scopeCluster = "scope-c1"
	scopeNS      = "fleet"
)

// keyMeta is the metadata of an object of instance key on the scope's
// connection (owner and cluster as given), with uid.
func keyMeta(name, key, owner, cluster, uid string) metav1.ObjectMeta {
	labels := names.Base(owner, cluster)
	labels[names.LabelKey] = key
	return metav1.ObjectMeta{Name: name, Namespace: scopeNS, Labels: labels, UID: types.UID(uid)}
}

func newScope(objs ...runtime.Object) (Scope, *fake.Clientset) {
	cs := fake.NewClientset(objs...)
	return Scope{Client: cs, Namespace: scopeNS, Owner: scopeOwner, Cluster: scopeCluster}, cs
}

// deletes are the "kind/name" of every delete the fake saw, sorted, with
// their propagation.
func deletes(cs *fake.Clientset) []string {
	var out []string
	for _, a := range cs.Actions() {
		if d, ok := a.(k8stesting.DeleteActionImpl); ok {
			p := ""
			if d.DeleteOptions.PropagationPolicy != nil {
				p = string(*d.DeleteOptions.PropagationPolicy)
			}
			out = append(out, d.GetResource().Resource+"/"+d.GetName()+" "+p)
		}
	}
	sort.Strings(out)
	return out
}

// A delete by key deletes exactly the key's objects on this connection of
// this daemon's, foreground, leaving one already being deleted alone.
func TestDeleteByKey(t *testing.T) {
	const key, other = "aaaaaaaa", "bbbbbbbb"
	now := metav1.Now()
	terminating := keyMeta("fi-a-one-aaaaaaaa-env-g1", key, scopeOwner, scopeCluster, "u-env")
	terminating.DeletionTimestamp, terminating.Finalizers = &now, []string{"x"}
	s, cs := newScope(
		&appsv1.StatefulSet{ObjectMeta: keyMeta("fi-a-one-aaaaaaaa", key, scopeOwner, scopeCluster, "u-sts")},
		&corev1.PersistentVolumeClaim{ObjectMeta: keyMeta("fi-a-one-aaaaaaaa", key, scopeOwner, scopeCluster, "u-pvc")},
		&corev1.Secret{ObjectMeta: keyMeta("fi-a-one-aaaaaaaa-spec-g1", key, scopeOwner, scopeCluster, "u-spec")},
		&corev1.Secret{ObjectMeta: terminating},
		&batchv1.Job{ObjectMeta: keyMeta("fi-a-one-aaaaaaaa-seed-g1", key, scopeOwner, scopeCluster, "u-job")},
		// The StatefulSet's pod goes with it; a bare pod of the key goes on its own.
		&corev1.Pod{ObjectMeta: func() metav1.ObjectMeta {
			m := keyMeta("fi-a-one-aaaaaaaa-0", key, scopeOwner, scopeCluster, "u-pod")
			m.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "fi-a-one-aaaaaaaa", UID: "u-sts"}}
			return m
		}()},
		&corev1.Pod{ObjectMeta: keyMeta("fi-a-one-aaaaaaaa-probe", key, scopeOwner, scopeCluster, "u-bare")},
		// Never: another key, another connection, another daemon.
		&corev1.PersistentVolumeClaim{ObjectMeta: keyMeta("fi-a-two-bbbbbbbb", other, scopeOwner, scopeCluster, "u-2")},
		&corev1.PersistentVolumeClaim{ObjectMeta: keyMeta("fi-a-one-aaaaaaaa-c2", key, scopeOwner, "scope-c2", "u-3")},
		&corev1.PersistentVolumeClaim{ObjectMeta: keyMeta("fi-a-one-aaaaaaaa-d2", key, "otherdaemon1", scopeCluster, "u-4")},
	)
	td, err := s.DeleteByKey(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"jobs/fi-a-one-aaaaaaaa-seed-g1 Foreground",
		"persistentvolumeclaims/fi-a-one-aaaaaaaa Foreground",
		"pods/fi-a-one-aaaaaaaa-probe Foreground",
		"secrets/fi-a-one-aaaaaaaa-spec-g1 Foreground",
		"statefulsets/fi-a-one-aaaaaaaa Foreground",
	}
	if got := deletes(cs); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("deleted:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(td.Deleted) != 5 || strings.Join(td.Remaining, ",") != "secret/fi-a-one-aaaaaaaa-env-g1" {
		t.Fatalf("teardown %+v", td)
	}
	// The disk goes last, after its users.
	if td.Deleted[len(td.Deleted)-1] != "pvc/fi-a-one-aaaaaaaa" || td.Deleted[0] != "sts/fi-a-one-aaaaaaaa" {
		t.Fatalf("order %v", td.Deleted)
	}
	if _, err := s.DeleteByKey(context.Background(), "../x"); err == nil {
		t.Fatal("a delete by something that is no key")
	}
}

// Without the owner Lease nothing is deleted.
func TestDeleteByKeyNeedsTheOwnerLease(t *testing.T) {
	s, cs := newScope(&corev1.PersistentVolumeClaim{ObjectMeta: keyMeta("fi-a-one-aaaaaaaa", "aaaaaaaa", scopeOwner, scopeCluster, "u-pvc")})
	s.Gate = func(context.Context) error { return ErrOwnerConflict }
	if _, err := s.DeleteByKey(context.Background(), "aaaaaaaa"); !errors.Is(err, ErrOwnerConflict) {
		t.Fatalf("err = %v", err)
	}
	if got := deletes(cs); len(got) != 0 {
		t.Fatalf("deleted without the Lease: %v", got)
	}
}

func markedMeta(name, key, uid, mark string) metav1.ObjectMeta {
	m := keyMeta(name, key, scopeOwner, scopeCluster, uid)
	if mark != "" {
		m.Annotations = map[string]string{names.AnnotationOp: mark}
	}
	return m
}

func rebuildJournal() Journal {
	return Journal{
		V: JournalVersion, Op: OpRebuild, Fleet: "alpha", Instance: "one", Cluster: scopeCluster, Key: "aaaaaaaa", Gen: 3,
		Step: "seed", PrevStep: StepReset, ResetStarted: true,
		Objects: []JournalObject{
			{Kind: KindStatefulSet, Name: "fi-a-one-aaaaaaaa", UID: "u-sts", Anchor: true},
			{Kind: KindSecret, Name: "fi-a-one-aaaaaaaa-spec-g3", UID: "u-spec3", Gen: 3, Created: true},
			{Kind: KindJob, Name: "fi-a-one-aaaaaaaa-seed-g3", Gen: 3, Created: true}, // its create never returned
		},
	}
}

func TestCheckJournal(t *testing.T) {
	ctx := context.Background()
	objs := func(stsUID, mark string) []runtime.Object {
		return []runtime.Object{
			&appsv1.StatefulSet{ObjectMeta: markedMeta("fi-a-one-aaaaaaaa", "aaaaaaaa", stsUID, mark)},
			&corev1.Secret{ObjectMeta: markedMeta("fi-a-one-aaaaaaaa-spec-g3", "aaaaaaaa", "u-spec3", "rebuild@reset")},
			&batchv1.Job{ObjectMeta: markedMeta("fi-a-one-aaaaaaaa-seed-g3", "aaaaaaaa", "u-job3", "rebuild@seed")},
		}
	}
	for _, tc := range []struct {
		name    string
		objs    []runtime.Object
		current bool
		why     string
		live    int
	}{
		{"current at its step", objs("u-sts", "rebuild@seed"), true, "", 3},
		{"mark still at the step before", objs("u-sts", "rebuild@reset"), true, "", 3},
		{"the job finished (mark gone)", objs("u-sts", ""), false, "the job finished", 3},
		{"anchor made again", objs("u-other", "rebuild@seed"), false, "another UID", 2},
		{"anchor gone", objs("u-sts", "rebuild@seed")[1:], false, "is gone", 2},
		{"someone else's object by that name", []runtime.Object{
			&appsv1.StatefulSet{ObjectMeta: markedMeta("fi-a-one-aaaaaaaa", "aaaaaaaa", "u-sts", "rebuild@seed")},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fi-a-one-aaaaaaaa-spec-g3", Namespace: scopeNS, UID: "u-spec3"}},
		}, false, "belongs to something else", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newScope(tc.objs...)
			c, err := s.CheckJournal(ctx, rebuildJournal())
			if err != nil {
				t.Fatal(err)
			}
			if c.Current != tc.current || !strings.Contains(c.Why, tc.why) || len(c.Live) != tc.live {
				t.Fatalf("check = current %v %q, %d live (%v)", c.Current, c.Why, len(c.Live), c.Live)
			}
		})
	}
}

// A journal's objects are deleted only as the journal knows them, and only
// those picked.
func TestDeleteJournalObjects(t *testing.T) {
	ctx := context.Background()
	s, cs := newScope(
		&appsv1.StatefulSet{ObjectMeta: markedMeta("fi-a-one-aaaaaaaa", "aaaaaaaa", "u-sts", "rebuild@seed")},
		&corev1.Secret{ObjectMeta: markedMeta("fi-a-one-aaaaaaaa-spec-g3", "aaaaaaaa", "u-spec3", "")},
		&batchv1.Job{ObjectMeta: markedMeta("fi-a-one-aaaaaaaa-seed-g3", "aaaaaaaa", "u-job3", "")},
		// The previous generation's spec, not the job's: kept.
		&corev1.Secret{ObjectMeta: markedMeta("fi-a-one-aaaaaaaa-spec-g2", "aaaaaaaa", "u-spec2", "")},
	)
	j := rebuildJournal()
	c, err := s.CheckJournal(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	td, err := s.DeleteJournalObjects(ctx, j, c, func(o JournalObject) bool { return o.Created })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(deletes(cs), ","); got != "jobs/fi-a-one-aaaaaaaa-seed-g3 Foreground,secrets/fi-a-one-aaaaaaaa-spec-g3 Foreground" || len(td.Deleted) != 2 {
		t.Fatalf("deleted %s (%+v)", got, td)
	}
}

func TestMirrorAndClearMark(t *testing.T) {
	ctx := context.Background()
	s, cs := newScope(
		&appsv1.StatefulSet{ObjectMeta: markedMeta("fi-a-one-aaaaaaaa", "aaaaaaaa", "u-sts", "rebuild@reset")},
		&corev1.Secret{ObjectMeta: markedMeta("fi-a-one-aaaaaaaa-spec-g3", "aaaaaaaa", "u-spec3", "rebuild@reset")},
	)
	j := rebuildJournal()
	if err := s.MirrorStep(ctx, j); err != nil {
		t.Fatal(err)
	}
	sts, _ := cs.AppsV1().StatefulSets(scopeNS).Get(ctx, "fi-a-one-aaaaaaaa", metav1.GetOptions{})
	if sts.Annotations[names.AnnotationOp] != "rebuild@seed" {
		t.Fatalf("anchor mark %v", sts.Annotations)
	}
	if err := s.ClearMark(ctx, j); err != nil {
		t.Fatal(err)
	}
	sts, _ = cs.AppsV1().StatefulSets(scopeNS).Get(ctx, "fi-a-one-aaaaaaaa", metav1.GetOptions{})
	sec, _ := cs.CoreV1().Secrets(scopeNS).Get(ctx, "fi-a-one-aaaaaaaa-spec-g3", metav1.GetOptions{})
	if _, ok := sts.Annotations[names.AnnotationOp]; ok {
		t.Fatalf("anchor still marked: %v", sts.Annotations)
	}
	if _, ok := sec.Annotations[names.AnnotationOp]; ok {
		t.Fatalf("object still marked: %v", sec.Annotations)
	}
	// A journal whose anchor was never written mirrors nothing.
	j.Objects[0].UID = ""
	before := len(cs.Actions())
	if err := s.MirrorStep(ctx, j); err != nil || len(cs.Actions()) != before {
		t.Fatalf("mirrored onto an unwritten anchor: %v", err)
	}
}

// A cluster without the snapshot API has no snapshots of fleet's.
func TestSnapshotsWithoutTheAPI(t *testing.T) {
	s, _ := newScope()
	k, _ := kindOf(KindVolumeSnapshot)
	if objs, err := k.list(context.Background(), s.Client, scopeNS, "x=y"); err != nil || len(objs) != 0 {
		t.Fatalf("snapshots %v %v", objs, err)
	}
	if _, err := k.get(context.Background(), s.Client, scopeNS, "snap"); !gone(err) {
		t.Fatalf("get: %v", err)
	}
}
