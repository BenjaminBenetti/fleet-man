//go:build !nok8s && linux

package kubernetes

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/jobboot"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// TestLiveTeardownAndUndo runs the teardown by key and the undo of a
// rebuild against a real cluster (FLEET_KUBE_TEST_CONTEXT, in
// fleet-it-p2-11-journal-* namespaces deleted when done):
//
//   - a create that failed after its seed (a bound disk, its Secrets, the
//     finished seed Job, a StatefulSet whose pod runs) is torn down by
//     DownByKey to zero labelled objects, its journal gone and its
//     tombstone kept;
//   - a rebuild interrupted past its reset is undone by the real rollback
//     tool Job (the pod build of fleet staged and fed by RunJob, podseed's
//     rollback putting generation 1's root back) and generation 1's
//     StatefulSet re-applied, stopped as it was.
func TestLiveTeardownAndUndo(t *testing.T) {
	kctx := os.Getenv("FLEET_KUBE_TEST_CONTEXT")
	if kctx == "" {
		t.Skip("set FLEET_KUBE_TEST_CONTEXT=<context> to run the teardown and undo against a cluster")
	}
	image := os.Getenv("FLEET_KUBE_TEST_IMAGE")
	if image == "" {
		image = "mcr.microsoft.com/devcontainers/base:debian-12"
	}
	t.Setenv("HOME", t.TempDir())
	lv := newLiveIn(t, kctx, "fleet-it-p2-11-journal-")
	art := lv.buildPod("vlive-1")
	pool, _ := lv.pool(art)
	ctx := context.Background()

	t.Run("failed after the seed", func(t *testing.T) {
		const key = "failedaa"
		sts := names.Instance("live", "failed", key)
		objs := lv.instanceObjects(sts, key, image, 1)
		jj, err := BeginJournal(Journal{Op: OpCreate, Fleet: "live", Instance: "failed", Cluster: lv.rec.ID, Namespace: lv.ns, Key: key, Gen: 1, Step: "verify", Objects: objs})
		if err != nil {
			t.Fatal(err)
		}
		lv.waitPodRunning(names.Pod(sts))

		td, err := pool.DownByKey(ctx, Tombstone{Cluster: lv.rec.ID, Key: key, Fleet: "live", Instance: "failed"})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("teardown deleted %v", td.Deleted)
		s, err := pool.Scope(lv.rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Minute)
		for {
			left, err := s.ListByKey(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			pods, err := lv.cs.CoreV1().Pods(lv.ns).List(ctx, metav1.ListOptions{LabelSelector: names.LabelKey + "=" + key})
			if err != nil {
				t.Fatal(err)
			}
			if len(left) == 0 && len(pods.Items) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("still labelled: %d objects, %d pods", len(left), len(pods.Items))
			}
			time.Sleep(2 * time.Second)
		}
		if _, err := os.Stat(jj.Path()); !os.IsNotExist(err) {
			t.Fatal("the journal is left")
		}
		fp, _ := LoadFootprint()
		if len(fp.TombstonesOf(lv.rec.ID)) != 1 {
			t.Fatalf("tombstones %+v", fp.Tombstones)
		}
	})

	t.Run("rebuild past its reset", func(t *testing.T) {
		const key = "rebuildb"
		sts := names.Instance("live", "rebuilt", key)
		objs := lv.instanceObjects(sts, key, image, 0)
		// Generation 2's half-made root, generation 1's in trash.
		lv.runPod(sts+"-prep", image, sts, `set -e
rm -rf /pvc/root /pvc/root.g1.trash /pvc/meta
mkdir -p /pvc/root/etc /pvc/root.g1.trash/etc /pvc/meta
echo two > /pvc/root/etc/gen
echo one > /pvc/root.g1.trash/etc/gen
echo '{"v":1,"gen":1}' > /pvc/meta/rootfs.g1.json`)
		gen1, err := podspec.ApplyBody(lv.statefulSet(sts, key, image, 1, 1))
		if err != nil {
			t.Fatal(err)
		}
		job := lv.rollbackJob(sts+"-rollback-g1", key, image, sts)
		jobBody, err := podspec.ApplyBody(job)
		if err != nil {
			t.Fatal(err)
		}
		zero := int32(0)
		if _, err := lv.cs.AppsV1().StatefulSets(lv.ns).Patch(ctx, sts, types.MergePatchType, annotationPatch(names.AnnotationOp, ptrString(MarkFor(OpRebuild, "seed"))), metav1.PatchOptions{FieldManager: DefaultFieldManager}); err != nil {
			t.Fatal(err)
		}
		j := Journal{
			V: JournalVersion, Op: OpRebuild, Fleet: "live", Instance: "rebuilt", Cluster: lv.rec.ID, Namespace: lv.ns, Key: key, Gen: 2,
			Step: "seed", PrevStep: StepReset, ResetStarted: true, PrevReplicas: &zero, PrevStatus: fleet.StatusStopped,
			Objects: objs, Rollback: &Rollback{StatefulSet: gen1, Job: jobBody},
		}
		for i := range j.Objects {
			j.Objects[i].Created = j.Objects[i].Kind == KindSecret && strings.HasSuffix(j.Objects[i].Name, "-g2")
			j.Objects[i].Anchor = j.Objects[i].Kind == KindStatefulSet
		}
		start := time.Now()
		undo, err := pool.UndoRebuild(ctx, j, func(s string) { t.Logf("undo: %s", s) })
		if err != nil {
			logs, _ := lv.cs.CoreV1().Pods(lv.ns).List(ctx, metav1.ListOptions{})
			t.Fatalf("undo: %v (pods %d)", err, len(logs.Items))
		}
		t.Logf("undone in %v: %+v", time.Since(start).Round(time.Millisecond), undo)
		if !undo.RolledBack {
			t.Fatal("not rolled back")
		}
		out := lv.runPod(sts+"-check", image, sts, `cat /pvc/root/etc/gen; ls /pvc; ls /pvc/meta`)
		if !strings.HasPrefix(out, "one\n") || strings.Contains(out, "root.g1.trash") || !strings.Contains(out, "rootfs.json") {
			t.Fatalf("the disk after the rollback:\n%s", out)
		}
		got, err := lv.cs.AppsV1().StatefulSets(lv.ns).Get(ctx, sts, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if *got.Spec.Replicas != 0 || got.Annotations[names.AnnotationRebuildGen] != "1" || got.Annotations[names.AnnotationOp] != "" {
			t.Fatalf("StatefulSet: replicas %d, annotations %v", *got.Spec.Replicas, got.Annotations)
		}
		if _, err := lv.cs.CoreV1().Secrets(lv.ns).Get(ctx, names.SpecSecret(sts, 2), metav1.GetOptions{}); err == nil {
			t.Fatal("generation 2's spec is left")
		}
		if _, err := lv.cs.BatchV1().Jobs(lv.ns).Get(ctx, job.Name, metav1.GetOptions{}); err == nil {
			deadline := time.Now().Add(time.Minute)
			for err == nil && time.Now().Before(deadline) {
				time.Sleep(time.Second)
				_, err = lv.cs.BatchV1().Jobs(lv.ns).Get(ctx, job.Name, metav1.GetOptions{})
			}
			if err == nil {
				t.Fatal("the rollback Job is left")
			}
		}
	})
}

func ptrString(s string) *string { return &s }

// keyLabels are the labels of an object of instance key on the live
// connection.
func (lv *live) keyLabels(key string, role names.Role) map[string]string {
	l := names.Base(liveOwner, lv.rec.ID)
	l[names.LabelKey] = key
	if role != "" {
		l[names.LabelRole] = string(role)
	}
	return l
}

// instanceObjects creates what a create that failed after its seed leaves:
// the disk, generation 1's (or 2's) Secrets, the finished seed Job, and the
// StatefulSet (replicas as given, mounting the disk), returning them as a
// journal records them.
func (lv *live) instanceObjects(sts, key, image string, replicas int32) []JournalObject {
	lv.t.Helper()
	ctx := context.Background()
	var objs []JournalObject
	record := func(kind, name string, uid string) {
		objs = append(objs, JournalObject{Kind: kind, Name: name, UID: uid, Gen: 1, Created: true, Anchor: kind == KindPVC})
	}
	pvc, err := lv.cs.CoreV1().PersistentVolumeClaims(lv.ns).Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: sts, Labels: lv.keyLabels(key, ""), Annotations: map[string]string{names.AnnotationOp: "create@verify"}},
		Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Mi")}}},
	}, metav1.CreateOptions{FieldManager: DefaultFieldManager})
	if err != nil {
		lv.t.Fatal(err)
	}
	record(KindPVC, pvc.Name, string(pvc.UID))
	for _, name := range []string{names.SpecSecret(sts, 1), names.EnvSecret(sts, 1), names.SpecSecret(sts, 2)} {
		sec, err := lv.cs.CoreV1().Secrets(lv.ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lv.keyLabels(key, "")}, Data: map[string][]byte{"pid1.json": []byte(`{"format":1}`)}}, metav1.CreateOptions{FieldManager: DefaultFieldManager})
		if err != nil {
			lv.t.Fatal(err)
		}
		record(KindSecret, sec.Name, string(sec.UID))
	}
	seed := lv.jobOf(names.SeedJob(sts, 1), key, image, sts, names.RoleSeed, []string{"sh", "-c", "mkdir -p /pvc/root && echo seeded"})
	got, err := lv.cs.BatchV1().Jobs(lv.ns).Create(ctx, seed, metav1.CreateOptions{FieldManager: DefaultFieldManager})
	if err != nil {
		lv.t.Fatal(err)
	}
	record(KindJob, got.Name, string(got.UID))
	set, err := lv.cs.AppsV1().StatefulSets(lv.ns).Create(ctx, lv.statefulSet(sts, key, image, replicas, 2), metav1.CreateOptions{FieldManager: DefaultFieldManager})
	if err != nil {
		lv.t.Fatal(err)
	}
	record(KindStatefulSet, set.Name, string(set.UID))
	// The seed ran to its end.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		j, err := lv.cs.BatchV1().Jobs(lv.ns).Get(ctx, got.Name, metav1.GetOptions{})
		if err == nil && j.Status.Succeeded > 0 {
			break
		}
		if time.Now().After(deadline) {
			lv.t.Fatalf("the seed Job did not finish: %v %+v", err, j.Status)
		}
		time.Sleep(time.Second)
	}
	return objs
}

// statefulSet is an instance-shaped StatefulSet of generation gen mounting
// the disk.
func (lv *live) statefulSet(sts, key, image string, replicas int32, gen int) *appsv1.StatefulSet {
	labels := lv.keyLabels(key, names.RoleInstance)
	no := false
	return &appsv1.StatefulSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{Name: sts, Labels: labels, Annotations: map[string]string{names.AnnotationRebuildGen: fmt.Sprint(gen)}},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas, ServiceName: sts,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: &no, TerminationGracePeriodSeconds: ptrInt64(1),
					Containers: []corev1.Container{{Name: DevContainer, Image: image, ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"sleep", "infinity"},
						VolumeMounts: []corev1.VolumeMount{{Name: "disk", MountPath: "/workspaces", SubPath: "ws"}, {Name: "spec", MountPath: "/opt/fleet/spec"}}}},
					Volumes: []corev1.Volume{
						{Name: "disk", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: sts}}},
						{Name: "spec", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: names.SpecSecret(sts, gen)}}},
					},
				},
			},
		},
	}
}

// jobOf is a Job of role mounting the disk at /pvc.
func (lv *live) jobOf(name, key, image, pvc string, role names.Role, command []string) *batchv1.Job {
	no := false
	backoff, ttl := int32(0), int32(600)
	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: lv.keyLabels(key, role)},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff, TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lv.keyLabels(key, role)},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: &no, TerminationGracePeriodSeconds: ptrInt64(1),
					Containers: []corev1.Container{{Name: podspec.JobContainer, Image: image, ImagePullPolicy: corev1.PullIfNotPresent, Command: command,
						VolumeMounts: []corev1.VolumeMount{{Name: "disk", MountPath: "/pvc"}, {Name: "tools", MountPath: jobboot.ToolsDir}}}},
					Volumes: []corev1.Volume{
						{Name: "disk", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc}}},
						{Name: "tools", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

// rollbackJob is the rollback tool Job a rebuild records: jobboot's wait
// loop, then pod-job, with the whole disk at podspec.JobDiskPath.
func (lv *live) rollbackJob(name, key, image, pvc string) *batchv1.Job {
	j := lv.jobOf(name, key, image, pvc, names.RoleTool, jobboot.Command(jobboot.Paths{}, 600))
	j.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath = podspec.JobDiskPath
	return j
}

// runPod runs script in a pod mounting the disk at /pvc and returns its
// output.
func (lv *live) runPod(name, image, pvc, script string) string {
	lv.t.Helper()
	ctx := context.Background()
	no := false
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: &no, TerminationGracePeriodSeconds: ptrInt64(1),
			Containers: []corev1.Container{{Name: "run", Image: image, ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"sh", "-c", script},
				VolumeMounts: []corev1.VolumeMount{{Name: "disk", MountPath: "/pvc"}}}},
			Volumes: []corev1.Volume{{Name: "disk", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc}}}},
		},
	}
	if _, err := lv.cs.CoreV1().Pods(lv.ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		lv.t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		p, err := lv.cs.CoreV1().Pods(lv.ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && (p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed) {
			raw, err := lv.cs.CoreV1().Pods(lv.ns).GetLogs(name, &corev1.PodLogOptions{}).DoRaw(ctx)
			if err != nil {
				lv.t.Fatal(err)
			}
			if p.Status.Phase == corev1.PodFailed {
				lv.t.Fatalf("pod %s failed:\n%s", name, raw)
			}
			_ = lv.cs.CoreV1().Pods(lv.ns).Delete(ctx, name, metav1.DeleteOptions{})
			lv.waitGone(name)
			return string(raw)
		}
		if time.Now().After(deadline) {
			lv.t.Fatalf("pod %s did not finish: %v", name, err)
		}
		time.Sleep(time.Second)
	}
}

func (lv *live) waitPodRunning(name string) {
	lv.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		p, err := lv.cs.CoreV1().Pods(lv.ns).Get(context.Background(), name, metav1.GetOptions{})
		if err == nil && p.Status.Phase == corev1.PodRunning {
			return
		}
		if time.Now().After(deadline) {
			lv.t.Fatalf("pod %s is not running: %v", name, err)
		}
		time.Sleep(time.Second)
	}
}
