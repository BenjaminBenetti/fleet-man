//go:build !nok8s

package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
)

// A ContainerID of a generation a rebuild replaced is missing, as a removed
// docker container is; the one fleetd's record runs is the pool's to say.
func TestStatusOfAReplacedGeneration(t *testing.T) {
	sts := handleSTS()
	r := newHandleRig(t)
	if err := SaveInstanceRecord(InstanceRecord{Cluster: handleCluster, Namespace: handleNS, Key: handleKey, StatefulSet: sts, Gen: 2}); err != nil {
		t.Fatal(err)
	}
	b := newHandle(r.inst(), r.pool)
	if got := b.Status(ContainerID(handleCluster, handleNS, sts, 1)); got != backend.LiveStatusMissing {
		t.Errorf("generation 1's status %v, want missing", got)
	}
	if got := b.Status(ContainerID(handleCluster, handleNS, sts, 2)); got == backend.LiveStatusMissing {
		t.Errorf("the current generation reads missing")
	}
	// Another instance's StatefulSet is not this record's to judge.
	if b.replacedGeneration(where{cluster: handleCluster, sts: "fi-other-x-zzzz2345", gen: 1}) {
		t.Error("judged another StatefulSet by this record")
	}
}

// The body a rollback re-applies is the StatefulSet as it was, without the
// job's mark or anything the API server keeps of its own.
func TestStatefulSetBody(t *testing.T) {
	one := int32(1)
	meta := handleMeta(handleSTS())
	meta.Annotations = map[string]string{names.AnnotationOp: "rebuild@prepare", names.AnnotationRebuildGen: "1"}
	meta.ResourceVersion, meta.Generation = "42", 3
	meta.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "fleet"}}
	sts := &appsv1.StatefulSet{ObjectMeta: meta, Spec: appsv1.StatefulSetSpec{Replicas: &one, ServiceName: "net"},
		Status: appsv1.StatefulSetStatus{Replicas: 1}}
	body, err := statefulSetBody(sts)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	md := m["metadata"].(map[string]any)
	annotations := md["annotations"].(map[string]any)
	if _, ok := annotations[names.AnnotationOp]; ok || annotations[names.AnnotationRebuildGen] != "1" {
		t.Errorf("annotations %v", annotations)
	}
	for _, k := range []string{"resourceVersion", "uid", "managedFields", "generation"} {
		if _, ok := md[k]; ok {
			t.Errorf("the body carries metadata.%s", k)
		}
	}
	if _, ok := m["status"]; ok || m["kind"] != "StatefulSet" || m["spec"].(map[string]any)["serviceName"] != "net" {
		t.Errorf("body %s", body)
	}
}

// A tool Job of the instance's generation runs the image its pods run, with
// their pull policy and Secrets.
func TestLiveImage(t *testing.T) {
	sts := &appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers:       []corev1.Container{{Name: "sidecar", Image: "other"}, {Name: DevContainer, Image: "reg/dev@sha256:aa", ImagePullPolicy: corev1.PullNever}},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "pull"}},
	}}}}
	got := liveImage(sts, InstanceRecord{Image: "recorded", ImageDigests: []string{"sha256:aa", "sha256:bb"}})
	if got.Ref != "reg/dev@sha256:aa" || got.PullPolicy != corev1.PullNever || got.ID != "sha256:aa" || len(got.PullSecrets) != 1 || got.PullSecrets[0] != "pull" {
		t.Errorf("liveImage = %+v", got)
	}
}

func TestGrownSize(t *testing.T) {
	const gi = int64(1 << 30)
	for _, tc := range []struct{ cur, add, want int64 }{
		{50 * gi, 1, 75 * gi},         // by half at least
		{50 * gi, 40 * gi, 90 * gi},   // by what is missing when more
		{10*gi + 1, 1, 16 * gi},       // in whole GiB
		{0, 3*gi + 5, 4 * gi},         // a claim without a size
		{20 * gi, 10*gi + 1, 31 * gi}, // rounded up
	} {
		if got := grownSize(tc.cur, tc.add); got != tc.want {
			t.Errorf("grownSize(%d, %d) = %d GiB, want %d GiB", tc.cur/gi, tc.add, got/gi, tc.want/gi)
		}
	}
}

func TestExportMode(t *testing.T) {
	for in, want := range map[string]string{"": RebuildExportAuto, "auto": RebuildExportAuto, "off": RebuildExportOff, " off ": RebuildExportOff, "tar": RebuildExportAuto} {
		if got := exportMode(in); got != want {
			t.Errorf("exportMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// rebuildRigOn is a rebuild of the rig's instance from generation 1 to 2,
// past its connect.
func rebuildRigOn(t *testing.T, r *handleRig, rec fleet.KubeCluster) *rebuilding {
	t.Helper()
	b := newHandle(r.inst(), r.pool)
	id, err := b.identity()
	if err != nil {
		t.Fatal(err)
	}
	rb := &rebuilding{creation: newCreation(b, id, backend.UpRequest{WorkspaceDir: r.inst().WorkspaceDir})}
	rb.cs, rb.ns, rb.owner, rb.rec = r.cs, handleNS, handleOwner, rec
	rb.gen, rb.prevGen, rb.created, rb.holder = 2, 1, time.Now(), rebuildHolder(handleKey, 2)
	rb.prev = InstanceRecord{Cluster: handleCluster, Namespace: handleNS, Key: handleKey, StatefulSet: id.sts, StatefulSetUID: "uid-" + id.sts, Gen: 1, WorkspaceFolder: "/workspaces/alpha"}
	return rb
}

// A disk short of room grows by what the new root lacks (by half at least),
// where the connection and the class allow it, and the rebuild waits for the
// cluster to take the size; anywhere else it fails before anything changed,
// saying why.
func TestGrowDisk(t *testing.T) {
	const gi = int64(1 << 30)
	yes, no := true, false
	disk := podseed.DiskSpace{Total: 50 * gi, Free: 2 * gi}
	for _, tc := range []struct {
		name      string
		autoOff   bool
		class     *storagev1.StorageClass
		className string
		want      string // "" grows
	}{
		{name: "grows", class: &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fast"}, AllowVolumeExpansion: &yes}, className: "fast"},
		{name: "auto expand off", autoOff: true, want: "auto_expand_disk is off"},
		{name: "no class", want: "no storage class"},
		{name: "class cannot grow", class: &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "local"}, AllowVolumeExpansion: &no}, className: "local", want: "storage class local cannot grow disks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sts := handleSTS()
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: handleMeta(sts), Spec: corev1.PersistentVolumeClaimSpec{
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")}}}}
			if tc.className != "" {
				pvc.Spec.StorageClassName = &tc.className
			}
			objs := []kruntime.Object{pvc}
			if tc.class != nil {
				objs = append(objs, tc.class)
			}
			r := newHandleRig(t, objs...)
			// The resizer: the claim's capacity follows its request.
			r.cs.PrependReactor("patch", "persistentvolumeclaims", func(a k8stesting.Action) (bool, kruntime.Object, error) {
				handled, obj, err := k8stesting.ObjectReaction(r.cs.Tracker())(a)
				if c, ok := obj.(*corev1.PersistentVolumeClaim); ok && err == nil {
					c.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: c.Spec.Resources.Requests[corev1.ResourceStorage]}
					_ = r.cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), c, handleNS)
				}
				return handled, obj, err
			})
			rec := fleet.KubeCluster{ID: handleCluster}
			if tc.autoOff {
				rec.AutoExpandDisk = &no
			}
			rb := rebuildRigOn(t, r, rec)
			rb.sts = &appsv1.StatefulSet{ObjectMeta: handleMeta(sts)}
			err := rb.growDisk(context.Background(), disk, 10*gi)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "2.0 GiB free") {
					t.Fatalf("growDisk = %v, want it to say %q and the room", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := r.cs.CoreV1().PersistentVolumeClaims(handleNS).Get(context.Background(), sts, metav1.GetOptions{})
			if q := got.Spec.Resources.Requests[corev1.ResourceStorage]; q.Value() != 75*gi {
				t.Errorf("the disk asks for %s, want 75Gi (grown by half)", q.String())
			}
			if len(rb.warnings) != 1 || !strings.Contains(rb.warnings[0], "grew from 50Gi to 75Gi") {
				t.Errorf("warnings %q", rb.warnings)
			}
		})
	}
}

// A rebuild's error says it is kubernetes:rebuild_failed and what became of
// the instance, the failed step's own code kept in its message.
func TestRebuildFailed(t *testing.T) {
	err := rebuildFailed(kubeerr.New(kubeerr.ImageBuild, "the build failed\nits output"), unchangedOutcome)
	code, detail, ok := kubeerr.Parse(err.Error())
	if !ok || code != kubeerr.RebuildFailed || detail != unchangedOutcome+": kubernetes:image_build: the build failed" {
		t.Errorf("rebuildFailed = %q", err)
	}
	if c, _ := kubeerr.Of(err); c != kubeerr.RebuildFailed {
		t.Errorf("Of = %s", c)
	}
	inner := errors.New("boom")
	if !errors.Is(rebuildFailed(inner, "x"), inner) {
		t.Error("the cause is not wrapped")
	}
	already := kubeerr.New(kubeerr.RebuildFailed, "undoing the failed rebuild failed")
	if got := rebuildFailed(already, "x"); got != already {
		t.Errorf("a rebuild_failed error is wrapped again: %v", got)
	}
}

// interruptedRebuildOn is a journaled rebuild of the rig's instance from
// generation 1, at step stop (before its reset): the StatefulSet carries the
// mark and was scaled down, generation 2's Secret was made.
func interruptedRebuildOn(t *testing.T, r *handleRig, rb *rebuilding) {
	t.Helper()
	sts := rb.id.sts
	zero, one := int32(0), int32(1)
	meta := handleMeta(sts)
	meta.Annotations = map[string]string{names.AnnotationOp: MarkFor(OpRebuild, stepStop)}
	secret := handleMeta(names.SpecSecret(sts, 2))
	for _, o := range []kruntime.Object{
		&appsv1.StatefulSet{ObjectMeta: meta, Spec: appsv1.StatefulSetSpec{Replicas: &zero}},
		&corev1.Secret{ObjectMeta: secret},
	} {
		if err := r.cs.Tracker().Add(o); err != nil {
			t.Fatal(err)
		}
	}
	prev := rb.prev
	jj, err := BeginJournal(Journal{
		Op: OpRebuild, Fleet: "alpha", Instance: "web", Cluster: handleCluster, Namespace: handleNS, Key: handleKey, Gen: 2, Step: stepStop,
		Objects: []JournalObject{
			{Kind: KindStatefulSet, Name: sts, UID: string(meta.UID), Gen: 1, Anchor: true},
			{Kind: KindSecret, Name: secret.Name, UID: string(secret.UID), Gen: 2, Created: true},
		},
		PrevReplicas: &one, PrevStatus: fleet.StatusRunning, PrevRecord: &prev,
	})
	if err != nil {
		t.Fatal(err)
	}
	rb.jj = jj
	rb.sts = &appsv1.StatefulSet{ObjectMeta: meta}
	// The record a later step would have written already.
	next := prev
	next.Gen = 2
	if err := SaveInstanceRecord(next); err != nil {
		t.Fatal(err)
	}
}

// A rebuild that fails after its journal began is undone at once: what it
// made goes, the replicas and fleetd's record come back, the journal ends,
// and the error says the generation was restored. A daemon shutdown leaves
// it at its journal instead (the next start undoes it); a destroy leaves it
// to the destroy.
func TestRebuildThatFailsAfterItsJournal(t *testing.T) {
	stepErr := errors.New("the devcontainer's lifecycle hooks: exit 7")
	for _, tc := range []struct {
		name  string
		cause error // nil: the step failed on its own
	}{
		{"undone", nil},
		{"daemon shutdown", backend.ErrDaemonShutdown},
		{"destroy", errors.New("instance destroyed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHandleRig(t)
			rb := rebuildRigOn(t, r, fleet.KubeCluster{ID: handleCluster})
			interruptedRebuildOn(t, r, rb)
			ctx := context.Background()
			if tc.cause != nil {
				c, cancel := context.WithCancelCause(ctx)
				cancel(tc.cause)
				ctx = c
			}
			err := rb.failed(ctx, stepErr)
			_, statErr := os.Stat(JournalPath(handleKey))
			set, _ := r.cs.AppsV1().StatefulSets(handleNS).Get(context.Background(), rb.id.sts, metav1.GetOptions{})
			rec, _ := LoadInstanceRecord(handleCluster, handleKey)
			secretLeft := true
			if _, gerr := r.cs.CoreV1().Secrets(handleNS).Get(context.Background(), names.SpecSecret(rb.id.sts, 2), metav1.GetOptions{}); gone(gerr) {
				secretLeft = false
			}
			switch tc.name {
			case "undone":
				if code, detail, _ := kubeerr.Parse(err.Error()); code != kubeerr.RebuildFailed || detail != "the rebuild failed; generation 1 was restored: the devcontainer's lifecycle hooks: exit 7" {
					t.Fatalf("err = %v", err)
				}
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("the journal outlived the undo: %v", statErr)
				}
				if *set.Spec.Replicas != 1 || set.Annotations[names.AnnotationOp] != "" {
					t.Errorf("StatefulSet after the undo: replicas %d annotations %v", *set.Spec.Replicas, set.Annotations)
				}
				if rec.Gen != 1 {
					t.Errorf("fleet's record after the undo is at generation %d", rec.Gen)
				}
				if secretLeft {
					t.Error("generation 2's Secret outlived the undo")
				}
			case "daemon shutdown":
				if !errors.Is(err, backend.ErrCheckpointed) || !strings.Contains(err.Error(), "at step stop") {
					t.Fatalf("err = %v, want the checkpoint naming the step", err)
				}
				if statErr != nil || *set.Spec.Replicas != 0 || !secretLeft || rec.Gen != 2 {
					t.Errorf("a checkpoint touched the rebuild: journal %v replicas %d secret %v gen %d", statErr, *set.Spec.Replicas, secretLeft, rec.Gen)
				}
			case "destroy":
				if err != stepErr {
					t.Fatalf("err = %v, want the step's own", err)
				}
				if !errors.Is(statErr, os.ErrNotExist) || set.Annotations[names.AnnotationOp] != "" {
					t.Errorf("a destroyed rebuild's journal %v, mark %q", statErr, set.Annotations[names.AnnotationOp])
				}
				if *set.Spec.Replicas != 0 {
					t.Error("a destroyed rebuild was undone")
				}
			}
		})
	}
}

// A rebuild's journal begins before its export: one that fails before it
// stopped the instance ends its journal (the StatefulSet's mark cleared) and
// says nothing changed; a daemon shutdown then leaves the journal, at its
// step and with the status the record had, for the next start to set back
// (generation 1 runs untouched); a destroy ends it.
func TestRebuildThatFailsBeforeItChangedTheInstance(t *testing.T) {
	exportErr := errors.New("the workspace export: the bulk session dropped")
	for _, tc := range []struct {
		name  string
		cause error // nil: the step failed on its own
	}{
		{"failed", nil},
		{"daemon shutdown", backend.ErrDaemonShutdown},
		{"destroy", errors.New("instance destroyed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHandleRig(t)
			rb := rebuildRigOn(t, r, fleet.KubeCluster{ID: handleCluster})
			one := int32(1)
			meta := handleMeta(rb.id.sts)
			rb.sts = &appsv1.StatefulSet{ObjectMeta: meta, Spec: appsv1.StatefulSetSpec{Replicas: &one}}
			rb.replicas = 1
			if err := r.cs.Tracker().Add(rb.sts.DeepCopy()); err != nil {
				t.Fatal(err)
			}
			if err := rb.begin(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := rb.step(context.Background(), "export", fleet.StepExportWorkspace, ""); err != nil {
				t.Fatal(err)
			}
			mark := func() string {
				set, err := r.cs.AppsV1().StatefulSets(handleNS).Get(context.Background(), rb.id.sts, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				return set.Annotations[names.AnnotationOp]
			}
			if got := mark(); got != MarkFor(OpRebuild, "export") {
				t.Fatalf("the StatefulSet's mark during the export: %q", got)
			}
			ctx := context.Background()
			if tc.cause != nil {
				c, cancel := context.WithCancelCause(ctx)
				cancel(tc.cause)
				ctx = c
			}
			err := rb.unchanged(ctx, exportErr)
			j, jerr := ReadJournal(JournalPath(handleKey))
			switch tc.name {
			case "daemon shutdown":
				if !errors.Is(err, backend.ErrCheckpointed) || !strings.Contains(err.Error(), "at step export") {
					t.Fatalf("err = %v, want the checkpoint naming the step", err)
				}
				if jerr != nil || j.Step != "export" || j.PrevStatus != fleet.StatusRunning || j.ResetStarted || j.PrevReplicas == nil || *j.PrevReplicas != 1 {
					t.Fatalf("the journal left for the next start: %+v, %v", j, jerr)
				}
				if got := mark(); got != MarkFor(OpRebuild, "export") {
					t.Errorf("the checkpoint cleared the mark: %q", got)
				}
			default:
				if code, detail, _ := kubeerr.Parse(err.Error()); code != kubeerr.RebuildFailed || !strings.HasPrefix(detail, unchangedOutcome) {
					t.Fatalf("err = %v", err)
				}
				if !errors.Is(jerr, os.ErrNotExist) {
					t.Errorf("the journal outlived a rebuild that changed nothing: %v", jerr)
				}
				if got := mark(); got != "" {
					t.Errorf("the StatefulSet keeps the mark %q", got)
				}
			}
		})
	}
}

// An instance whose StatefulSet was deleted outside fleet, its disk kept, is
// rebuilt as a stopped one over that disk: load takes it (with a warning),
// and the journal's anchor is the StatefulSet the rebuild makes, with no
// replicas to restore. With its disk gone too, there is nothing to rebuild.
func TestRebuildOfAnInstanceWhoseStatefulSetIsGone(t *testing.T) {
	r := newHandleRig(t)
	rb := rebuildRigOn(t, r, fleet.KubeCluster{ID: handleCluster})
	if err := SaveInstanceRecord(rb.prev); err != nil {
		t.Fatal(err)
	}
	if err := r.cs.Tracker().Add(&corev1.PersistentVolumeClaim{ObjectMeta: handleMeta(rb.id.sts)}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := rb.load(ctx); err != nil {
		t.Fatalf("load of an instance whose StatefulSet is gone: %v", err)
	}
	if !rb.lostSTS || rb.sts != nil || rb.running() {
		t.Fatalf("lost %v sts %v running %v", rb.lostSTS, rb.sts, rb.running())
	}
	if w := strings.Join(rb.warningLines(), "\n"); !strings.Contains(w, "deleted outside fleet") {
		t.Errorf("no warning that the StatefulSet is gone: %q", w)
	}
	if err := rb.begin(ctx); err != nil {
		t.Fatal(err)
	}
	j := rb.jj.Journal()
	if a, ok := j.Anchor(); !ok || a.Name != rb.id.sts || !a.Created || a.UID != "" || j.PrevReplicas != nil || j.PrevStatus != fleet.StatusStopped {
		t.Fatalf("journal %+v", j)
	}
	rb.endJournal()

	gone := rebuildRigOn(t, newHandleRig(t), fleet.KubeCluster{ID: handleCluster})
	if err := SaveInstanceRecord(gone.prev); err != nil {
		t.Fatal(err)
	}
	err := gone.load(context.Background())
	if code, _ := kubeerr.Of(err); err == nil || !strings.Contains(err.Error(), "nothing of it is left to rebuild") || code != kubeerr.RebuildFailed {
		t.Fatalf("an instance with neither StatefulSet nor disk: %v", err)
	}
}

// An undo that fails keeps the journal, and its error starts the way the
// reconciler claims such a record by at the next start.
func TestRebuildWhoseUndoFails(t *testing.T) {
	r := newHandleRig(t)
	rb := rebuildRigOn(t, r, fleet.KubeCluster{ID: handleCluster})
	interruptedRebuildOn(t, r, rb)
	r.cs.PrependReactor("patch", "statefulsets", func(a k8stesting.Action) (bool, kruntime.Object, error) {
		if strings.Contains(string(a.(k8stesting.PatchAction).GetPatch()), `"replicas"`) {
			return true, nil, errors.New("the API server is gone")
		}
		return false, nil, nil
	})
	err := rb.failed(context.Background(), errors.New("seed failed"))
	if !strings.HasPrefix(err.Error(), "kubernetes:rebuild_failed: undoing") || !strings.Contains(err.Error(), "seed failed") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(JournalPath(handleKey)); statErr != nil {
		t.Fatalf("the journal of a rebuild whose undo failed is gone: %v", statErr)
	}
}

// The retry of a create that never ran deletes what it left by key (no
// tombstone: the instance stays), forgets fleetd's record of its pod, and
// creates again.
func TestRetryCreateDeletesWhatTheCreateLeft(t *testing.T) {
	sts := handleSTS()
	r := newHandleRig(t,
		&appsv1.StatefulSet{ObjectMeta: handleMeta(sts)},
		&corev1.PersistentVolumeClaim{ObjectMeta: handleMeta(sts)},
		&corev1.Secret{ObjectMeta: handleMeta(names.SpecSecret(sts, 1))},
		&batchv1.Job{ObjectMeta: handleMeta(names.SeedJob(sts, 1))},
	)
	if err := SaveInstanceRecord(InstanceRecord{Cluster: handleCluster, Namespace: handleNS, Key: handleKey, StatefulSet: sts, Gen: 1}); err != nil {
		t.Fatal(err)
	}
	inst := r.inst()
	inst.Status = fleet.StatusFailed
	b := newHandle(inst, r.pool)
	// No daemon environment: the create itself stops at once; what the
	// retry did before it is the point.
	_, err := b.RebuildContext(context.Background(), "", backend.UpRequest{WorkspaceDir: inst.WorkspaceDir})
	if !errors.Is(err, errNotDaemon) {
		t.Fatalf("the retried create: %v", err)
	}
	s, _ := r.pool.Scope(handleCluster)
	if left, _ := s.ListByKey(context.Background(), handleKey); len(left) != 0 {
		t.Errorf("the failed create's objects outlived its retry: %v", left)
	}
	if _, err := LoadInstanceRecord(handleCluster, handleKey); !errors.Is(err, errNoInstanceRecord) {
		t.Errorf("fleet's record of the failed create's pod: %v", err)
	}
	if fp, _ := LoadFootprint(); len(fp.TombstonesOf(handleCluster)) != 0 {
		t.Error("a retry tombstoned the instance's key")
	}
}

// An export that arrived whole replaces the daemon host's checkout (what the
// pod deleted goes from it too); one that did not leaves the checkout as it
// was.
func TestFinishExport(t *testing.T) {
	pod := t.TempDir()
	writeTree(t, pod, map[string]string{".devcontainer/devcontainer.json": `{"image":"edited"}`, "src/main.go": "package main\n"})

	for _, tc := range []struct {
		name    string
		mangle  func(*podseed.ExportSummary)
		werr    error
		noSum   bool
		want    string
		swapped bool
	}{
		{name: "whole", swapped: true},
		{name: "damaged", mangle: func(s *podseed.ExportSummary) { s.SHA256 = "00" }, want: "arrived damaged"},
		{name: "short", mangle: func(s *podseed.ExportSummary) { s.Size++ }, want: "arrived damaged"},
		{name: "no summary", noSum: true, want: "without its summary"},
		{name: "the export failed", werr: errors.New("exit code 2"), want: "the workspace export: exit code 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHandleRig(t)
			rb := rebuildRigOn(t, r, fleet.KubeCluster{ID: handleCluster})
			ws := rb.req.WorkspaceDir
			writeTree(t, ws, map[string]string{".devcontainer/devcontainer.json": `{"image":"old"}`, "deleted-in-the-pod": "x"})
			var tarBuf bytes.Buffer
			sum, err := podseed.ExportWorkspace(&tarBuf, pod, podseed.ExportOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := rb.receive(context.Background(), &tarBuf)
			if err != nil {
				t.Fatal(err)
			}
			if tc.mangle != nil {
				tc.mangle(&sum)
			}
			stderr := podseed.FormatExportSummary(sum) + "\n"
			if tc.noSum {
				stderr = "killed\n"
			}
			err = rb.finishExport(context.Background(), got, nil, tc.werr, stderr)
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("finishExport = %v, want %q", err, tc.want)
			}
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filepath.Join(ws, ".devcontainer/devcontainer.json"))
			_, deletedErr := os.Stat(filepath.Join(ws, "deleted-in-the-pod"))
			if tc.swapped != (string(data) == `{"image":"edited"}`) || tc.swapped != errors.Is(deletedErr, os.ErrNotExist) {
				t.Errorf("checkout after the export: devcontainer.json %s, deleted file %v (swapped wanted %v)", data, deletedErr, tc.swapped)
			}
			for _, dir := range []string{exportDir(ws), oldDir(ws)} {
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s left behind: %v", dir, err)
				}
			}
		})
	}
}

// A rebuild with no checkout on the daemon host (a clone's) takes the export
// as the checkout.
func TestSwapCheckoutWithoutOne(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "ws")
	export := exportDir(ws)
	writeTree(t, export, map[string]string{"a": "1"})
	if err := swapCheckout(ws, export); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(ws, "a")); err != nil || string(data) != "1" {
		t.Errorf("checkout %q %v", data, err)
	}
}

// A tool Job of the instance's generation runs where its pods run (node
// selector, runtime class), as a tool, asking for little.
func TestToolJobOfTheGeneration(t *testing.T) {
	rc := "gvisor"
	sts := &appsv1.StatefulSet{ObjectMeta: handleMeta(handleSTS()), Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		NodeSelector: map[string]string{"pool": "dev"}, RuntimeClassName: &rc,
		Containers: []corev1.Container{{Name: DevContainer, Image: "reg/dev@sha256:aa"}},
	}}}}
	in := podspec.RenderInput{
		Cluster: &fleet.KubeCluster{ID: handleCluster}, Namespace: handleNS, Name: sts.Name, Key: handleKey, Owner: handleOwner, ClusterID: handleCluster,
		FleetName: "alpha", FleetHash: "abcd2345", InstanceName: "web", Gen: 1, Image: liveImage(sts, InstanceRecord{}),
	}
	job, err := podspec.RenderJob(toolResult(sts), in, podspec.JobInput{Name: names.ExportJob(sts.Name, 1), Resources: toolResources()}, names.RoleTool)
	if err != nil {
		t.Fatal(err)
	}
	spec := job.Job.Spec.Template.Spec
	if spec.NodeSelector["pool"] != "dev" || spec.RuntimeClassName == nil || *spec.RuntimeClassName != "gvisor" {
		t.Errorf("placement %v %v", spec.NodeSelector, spec.RuntimeClassName)
	}
	c := spec.Containers[0]
	if c.Image != "reg/dev@sha256:aa" || c.Resources.Requests.Memory().String() != "128Mi" || job.Job.Labels[names.LabelRole] != string(names.RoleTool) {
		t.Errorf("container %s %v labels %v", c.Image, c.Resources, job.Job.Labels)
	}
}

// The rollback Job runs where the generation it restores ran: the new
// generation's placement (another architecture, a runtime class) may be what
// the rebuild failed on, and the disk is on a node the old one fits.
func TestRollbackJobRunsWhereTheGenerationRan(t *testing.T) {
	r := newHandleRig(t)
	rb := rebuildRigOn(t, r, fleet.KubeCluster{ID: handleCluster})
	sts := rb.id.sts
	zero := int32(0)
	rb.sts = &appsv1.StatefulSet{ObjectMeta: handleMeta(sts), Spec: appsv1.StatefulSetSpec{Replicas: &zero, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		NodeSelector: map[string]string{"kubernetes.io/arch": "amd64"},
		Containers:   []corev1.Container{{Name: DevContainer, Image: "reg/dev@sha256:aa"}},
	}}}}
	jj, err := BeginJournal(Journal{Op: OpRebuild, Fleet: "alpha", Instance: "web", Cluster: handleCluster, Namespace: handleNS, Key: handleKey, Gen: 2, Step: stepApply,
		Objects: []JournalObject{{Kind: KindStatefulSet, Name: sts, UID: "uid-" + sts, Gen: 1, Anchor: true}}})
	if err != nil {
		t.Fatal(err)
	}
	rb.jj = jj
	in := podspec.RenderInput{
		Cluster: &fleet.KubeCluster{ID: handleCluster}, Namespace: handleNS, Name: sts, Key: handleKey, Owner: handleOwner, ClusterID: handleCluster,
		FleetName: "alpha", FleetHash: "abcd2345", InstanceName: "web", Gen: 2, Image: podspec.ImageRef{Ref: "reg/new@sha256:bb"},
	}
	if err := rb.recordRollback(in); err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := json.Unmarshal(rb.jj.Journal().Rollback.Job, &job); err != nil {
		t.Fatal(err)
	}
	spec := job.Spec.Template.Spec
	if spec.NodeSelector["kubernetes.io/arch"] != "amd64" || spec.RuntimeClassName != nil {
		t.Errorf("the rollback Job runs at %v (runtime class %v), not where generation 1 ran", spec.NodeSelector, spec.RuntimeClassName)
	}
	if job.Name != names.RollbackJob(sts, 1) || spec.Containers[0].Image != "reg/dev@sha256:aa" {
		t.Errorf("rollback Job %s runs %s", job.Name, spec.Containers[0].Image)
	}
}

// A checkout the export replaces is deleted even where it holds read-only
// dirs (a Go module cache), as is one an earlier rebuild left on its way
// out: either would otherwise fail every later rebuild.
func TestSwapCheckoutOverReadOnlyDirs(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "ws")
	readOnly := func(root string) {
		writeTree(t, root, map[string]string{"pkg/mod/m@v1/go.mod": "module m\n"})
		if err := os.Chmod(filepath.Join(root, "pkg/mod/m@v1"), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	readOnly(ws)
	readOnly(oldDir(ws))
	export := exportDir(ws)
	writeTree(t, export, map[string]string{"a": "1"})
	if err := swapCheckout(ws, export); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(ws, "a")); err != nil || string(data) != "1" {
		t.Errorf("checkout %q %v", data, err)
	}
	if _, err := os.Stat(oldDir(ws)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the replaced checkout is still there: %v", err)
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
