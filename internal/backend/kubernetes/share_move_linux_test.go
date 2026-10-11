//go:build !nok8s && linux

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// moveRig is a job harness with fleet alpha's share on it, in class
// "standard" (single-node mode), and one stopped instance, web.
type moveRig struct {
	*jobHarness
	ident     owner.Identity
	h8, claim string
	copyClaim string
	sts       string
	caps      *fleetgrpc.KubeCapabilities
}

func newMoveRig(t *testing.T, oldMode topology.Mode) *moveRig {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	h := newJobHarness(t)
	ident, err := owner.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	r := &moveRig{jobHarness: h, ident: ident}
	r.h8 = names.FleetHash(ident.DaemonID, testCluster, "alpha")
	r.claim, r.copyClaim = ShareClaim("alpha", r.h8), ShareMoveClaim("alpha", r.h8)
	r.sts = names.Instance("alpha", "web", rbKey)
	r.caps = &fleetgrpc.KubeCapabilities{
		ShareMode: string(topology.MultiNode), SharedStorageClass: "nfs", InstanceStorageClass: "standard",
		StorageClasses: []*fleetgrpc.KubeStorageClass{
			{Name: "standard", Provisioner: "rancher.io/local-path", IsDefault: true, NodeLocal: true, Block: true, BindingMode: "WaitForFirstConsumer"},
			{Name: "nfs", Provisioner: "nfs.csi.k8s.io", BindingMode: "Immediate"},
		},
		Nodes: []*fleetgrpc.KubeNode{{Name: "node-a"}, {Name: "node-b"}},
	}
	SetEnvironment(Environment{Facts: func(context.Context, string) (Facts, error) { return Facts{Capabilities: r.caps}, nil }})
	t.Cleanup(func() { SetEnvironment(Environment{}) })
	old := []any{shareMoveInterval, shareGoneWait}
	shareMoveInterval, shareGoneWait = 5*time.Millisecond, 5*time.Second
	t.Cleanup(func() { shareMoveInterval, shareGoneWait = old[0].(time.Duration), old[1].(time.Duration) })

	standard := "standard"
	terms, _ := json.Marshal([]corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{"node-a"}}}}})
	share := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: r.claim, Namespace: jobNS, UID: "share-uid-1", Labels: shareLabels(ident.Owner(), testCluster, r.h8),
			Annotations: map[string]string{names.AnnotationShareMode: string(oldMode), names.AnnotationShareTopology: string(terms), names.AnnotationVolumesUnused: "a1b2c3d4"}},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &standard, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("15Gi")}},
	}
	placement := topology.Placement{Mode: oldMode, ShareGroup: r.h8}
	_ = json.Unmarshal(terms, &placement.Terms)
	labels := names.Base(ident.Owner(), testCluster)
	labels[names.LabelKey], labels[names.LabelRole], labels[names.LabelShareGroup], labels[names.LabelFleet] = rbKey, string(names.RoleInstance), r.h8, r.h8
	zero := int32(0)
	set := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: r.sts, Namespace: jobNS, Labels: labels, Annotations: map[string]string{names.AnnotationRebuildGen: "1"}},
		Spec: appsv1.StatefulSetSpec{Replicas: &zero, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				Affinity:   placement.Affinity(true),
				Containers: []corev1.Container{{Name: DevContainer, Image: "registry.test/dev@sha256:" + strings.Repeat("a", 64)}},
				Volumes:    []corev1.Volume{{Name: "share", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: r.claim}}}},
			},
		}},
	}
	execSpec, _ := json.Marshal(map[string]any{"remote_user": "vscode"})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.SpecSecret(r.sts, 1), Namespace: jobNS}, Data: map[string][]byte{podspec.ExecFile: execSpec}}
	h.seed([]runtime.Object{share, set, secret}...)
	if err := SaveInstanceRecord(InstanceRecord{Cluster: testCluster, Namespace: jobNS, Key: rbKey, Fleet: "alpha", Instance: "web", StatefulSet: r.sts, Gen: 1,
		Share: &ShareRecord{Mode: string(oldMode), Claim: r.claim, Roots: []podshare.SpokeRoot{{Name: ".claude"}, {Name: ".codex"}}}}); err != nil {
		t.Fatal(err)
	}
	return r
}

// setShared sets the connection's shared class, as kube_clusters.json
// holds it (the record a move reads).
func (r *moveRig) setShared(t *testing.T, class string) {
	t.Helper()
	c, err := r.pool.Cluster(testCluster)
	if err != nil {
		t.Fatal(err)
	}
	rec := c.Record
	rec.SharedStorageClass = class
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Clusters = []fleet.KubeCluster{rec}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (r *moveRig) pvc(name string) *corev1.PersistentVolumeClaim {
	pvc, err := r.cs.CoreV1().PersistentVolumeClaims(jobNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return pvc
}

// TestMoveShare: a stopped fleet's single-node share moves to the new
// shared class (one that passed the multi-node test: multi-node mode) by
// two copy Jobs through the move's copy; the share keeps its name, takes
// the class, its mode and size, and every instance is placed anew (no
// share-holder affinity, Codex's SQLite on its own disk, the mode on its
// record); the copy goes.
func TestMoveShare(t *testing.T) {
	r := newMoveRig(t, topology.SingleNode)
	ctx := context.Background()
	mv, err := r.pool.ShareMoveOf(ctx, testCluster, "alpha")
	if err != nil || mv == nil || mv.From != "standard" || mv.To != "nfs" || mv.Mode != topology.MultiNode || mv.Blocked != "" || mv.Begun {
		t.Fatalf("ShareMoveOf = %+v, %v", mv, err)
	}
	var steps []string
	if err := r.pool.MoveShare(ctx, testCluster, "alpha", func(s string) { steps = append(steps, s) }); err != nil {
		t.Fatal(err)
	}
	share := r.pvc(r.claim)
	if share == nil || classOf(share) != "nfs" || share.UID == "share-uid-1" || share.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Fatalf("the new share: %+v", share)
	}
	if share.Annotations[names.AnnotationShareMode] != string(topology.MultiNode) || share.Annotations[names.AnnotationShareTopology] != "[]" || share.Annotations[names.AnnotationVolumesUnused] != "a1b2c3d4" {
		t.Fatalf("the new share's annotations: %v", share.Annotations)
	}
	if _, moving := share.Annotations[names.AnnotationShareMove]; moving {
		t.Fatal("the new share is still marked moving")
	}
	if q := share.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "15Gi" {
		t.Fatalf("the new share is %s, the old one held 15Gi", q.String())
	}
	if r.pvc(r.copyClaim) != nil {
		t.Fatal("the move's copy is left")
	}
	r.mu.Lock()
	specs := append([]podseed.JobSpec(nil), r.specs...)
	r.mu.Unlock()
	if len(specs) != 2 {
		t.Fatalf("copy Jobs ran %d times", len(specs))
	}
	for _, s := range specs {
		if s.Op != podseed.OpShareCopy || s.ShareCopy == nil || s.ShareCopy.Source != shareCopySource || s.PVC != podspec.JobDiskPath {
			t.Fatalf("copy spec %+v", s)
		}
	}
	if strings.Join(steps, ",") != "copying the share to a nfs volume,deleting the share in standard,filling the new share,placing the fleet's instances" {
		t.Fatalf("steps %q", steps)
	}
	set, err := r.cs.AppsV1().StatefulSets(jobNS).Get(ctx, r.sts, metav1.GetOptions{})
	if err != nil || set.Spec.Template.Spec.Affinity != nil {
		t.Fatalf("the instance is still placed by the old share: %v %v", set.Spec.Template.Spec.Affinity, err)
	}
	sec, _ := r.cs.CoreV1().Secrets(jobNS).Get(ctx, names.SpecSecret(r.sts, 1), metav1.GetOptions{})
	var exec map[string]any
	_ = json.Unmarshal(sec.Data[podspec.ExecFile], &exec)
	if home, _ := exec["home_env"].(map[string]any); home[podspec.CodexSQLiteHomeEnv] != podspec.CodexSQLiteHome || exec["remote_user"] != "vscode" {
		t.Fatalf("exec spec %v", exec)
	}
	rec, err := LoadInstanceRecord(testCluster, rbKey)
	if err != nil || rec.Share.Mode != string(topology.MultiNode) || rec.Share.Claim != r.claim {
		t.Fatalf("record %+v %v", rec.Share, err)
	}
	if mv, err := r.pool.ShareMoveOf(ctx, testCluster, "alpha"); err != nil || mv != nil {
		t.Fatalf("after the move: %+v %v", mv, err)
	}
}

// A share stays single-node when the new class does not pass the
// multi-node test; its instances keep their share-holder affinity, by the
// node the copies ran on.
func TestMoveShareKeepsSingleNodeMode(t *testing.T) {
	r := newMoveRig(t, topology.SingleNode)
	r.caps.ShareMode, r.caps.SharedStorageClass = string(topology.SingleNode), "fast"
	r.caps.StorageClasses = append(r.caps.StorageClasses, &fleetgrpc.KubeStorageClass{Name: "fast", Provisioner: "ebs.csi.aws.com", Block: true, BindingMode: "WaitForFirstConsumer"})
	r.setShared(t, "fast")
	ctx := context.Background()
	if err := r.pool.MoveShare(ctx, testCluster, "alpha", nil); err != nil {
		t.Fatal(err)
	}
	share := r.pvc(r.claim)
	if classOf(share) != "fast" || share.Spec.AccessModes[0] != corev1.ReadWriteOnce || share.Annotations[names.AnnotationShareMode] != string(topology.SingleNode) {
		t.Fatalf("the new share: %v %v", share.Spec, share.Annotations)
	}
	set, _ := r.cs.AppsV1().StatefulSets(jobNS).Get(ctx, r.sts, metav1.GetOptions{})
	if a := set.Spec.Template.Spec.Affinity; a == nil || a.PodAffinity == nil {
		t.Fatalf("a single-node instance lost its share-holder affinity: %v", a)
	}
	sec, _ := r.cs.CoreV1().Secrets(jobNS).Get(ctx, names.SpecSecret(r.sts, 1), metav1.GetOptions{})
	if strings.Contains(string(sec.Data[podspec.ExecFile]), podspec.CodexSQLiteHomeEnv) {
		t.Fatal("a single-node share moved Codex's SQLite home")
	}
}

// A move refuses a fleet that runs, or a share a pod mounts; a multi-node
// share does not move to a class used from one node at a time.
func TestMoveShareRefuses(t *testing.T) {
	ctx := context.Background()
	t.Run("running", func(t *testing.T) {
		r := newMoveRig(t, topology.SingleNode)
		set, _ := r.cs.AppsV1().StatefulSets(jobNS).Get(ctx, r.sts, metav1.GetOptions{})
		one := int32(1)
		set.Spec.Replicas = &one
		_, _ = r.cs.AppsV1().StatefulSets(jobNS).Update(ctx, set, metav1.UpdateOptions{})
		if err := r.pool.MoveShare(ctx, testCluster, "alpha", nil); !errors.Is(err, ErrShareInUse) {
			t.Fatalf("err = %v", err)
		}
		if classOf(r.pvc(r.claim)) != "standard" || r.pvc(r.copyClaim) != nil {
			t.Fatal("a refused move touched the share")
		}
	})
	t.Run("mounted", func(t *testing.T) {
		r := newMoveRig(t, topology.SingleNode)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "stray", Namespace: jobNS, Labels: names.Base(r.ident.Owner(), testCluster)},
			Spec:   corev1.PodSpec{Volumes: []corev1.Volume{{Name: "s", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: r.claim}}}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		r.seed(pod)
		if err := r.pool.MoveShare(ctx, testCluster, "alpha", nil); !errors.Is(err, ErrShareInUse) || !strings.Contains(err.Error(), "stray") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("multi-node to one node", func(t *testing.T) {
		r := newMoveRig(t, topology.MultiNode)
		r.caps.ShareMode, r.caps.SharedStorageClass = string(topology.SingleNode), "fast"
		r.caps.StorageClasses = append(r.caps.StorageClasses, &fleetgrpc.KubeStorageClass{Name: "fast", Provisioner: "ebs.csi.aws.com", Block: true, BindingMode: "WaitForFirstConsumer"})
		r.setShared(t, "fast")
		mv, err := r.pool.ShareMoveOf(ctx, testCluster, "alpha")
		if err != nil || mv == nil || !strings.Contains(mv.Blocked, "single-node mode") {
			t.Fatalf("ShareMoveOf = %+v %v", mv, err)
		}
		if code, _ := kubeerr.Of(r.pool.MoveShare(ctx, testCluster, "alpha", nil)); code != kubeerr.Misconfigured.Narrow("share_move") {
			t.Fatalf("code = %v", code)
		}
	})
	t.Run("where it belongs", func(t *testing.T) {
		r := newMoveRig(t, topology.SingleNode)
		r.setShared(t, "standard")
		if mv, err := r.pool.ShareMoveOf(ctx, testCluster, "alpha"); err != nil || mv != nil {
			t.Fatalf("ShareMoveOf = %+v %v", mv, err)
		}
	})
	t.Run("Auto in single-node mode follows no disk class", func(t *testing.T) {
		// No class passed: Auto puts shares in the instance disks' class,
		// and a later change of that moves nothing.
		r := newMoveRig(t, topology.SingleNode)
		r.caps.ShareMode, r.caps.SharedStorageClass, r.caps.InstanceStorageClass = string(topology.SingleNode), "nfs", "nfs"
		if mv, err := r.pool.ShareMoveOf(ctx, testCluster, "alpha"); err != nil || mv != nil {
			t.Fatalf("ShareMoveOf = %+v %v", mv, err)
		}
	})
}

// A start's gate calls off a move an earlier daemon left before deleting
// the old share (the share is as it was), and refuses one past it until a
// pass finishes it.
func TestShareMoveGate(t *testing.T) {
	ctx := context.Background()
	r := newMoveRig(t, topology.SingleNode)
	info := shareMoveInfo{From: "standard", To: "nfs", Mode: topology.MultiNode, OldMode: topology.SingleNode, Phase: moveCopied, Size: "15Gi"}
	nfs := "nfs"
	copyPVC := func() *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: r.copyClaim, Namespace: jobNS, UID: "copy-uid", Labels: shareLabels(r.ident.Owner(), testCluster, r.h8),
				Annotations: map[string]string{names.AnnotationShareMove: info.annotation()}},
			Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &nfs},
		}
	}
	r.seed(copyPVC())
	if mv, _ := r.pool.ShareMoveOf(ctx, testCluster, "alpha"); mv == nil || !mv.Begun || mv.Committed {
		t.Fatalf("a begun move: %+v", mv)
	}
	if err := r.pool.ShareMoveGate(ctx, testCluster, "alpha"); err != nil {
		t.Fatalf("the gate did not call the move off: %v", err)
	}
	if r.pvc(r.copyClaim) != nil || classOf(r.pvc(r.claim)) != "standard" {
		t.Fatal("calling it off did not leave the share as it was")
	}

	// Past the point of no return: the old share is gone.
	r.seed(copyPVC())
	if err := r.cs.CoreV1().PersistentVolumeClaims(jobNS).Delete(ctx, r.claim, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if mv, _ := r.pool.ShareMoveOf(ctx, testCluster, "alpha"); mv == nil || !mv.Committed {
		t.Fatalf("a committed move: %+v", mv)
	}
	err := r.pool.ShareMoveGate(ctx, testCluster, "alpha")
	if code, _ := kubeerr.Of(err); code != kubeerr.Unavailable.Narrow("share_moving") {
		t.Fatalf("the gate let a start through a half-moved share: %v", err)
	}
	// A pass finishes it: the share comes back, filled from the copy.
	if err := r.pool.MoveShare(ctx, testCluster, "alpha", nil); err != nil {
		t.Fatal(err)
	}
	if classOf(r.pvc(r.claim)) != "nfs" || r.pvc(r.copyClaim) != nil {
		t.Fatal("the pass did not finish the move")
	}
	r.mu.Lock()
	n := len(r.specs)
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("a finished move ran %d copy Jobs, want the second alone", n)
	}
	if err := r.pool.ShareMoveGate(ctx, testCluster, "alpha"); err != nil {
		t.Fatalf("after the move: %v", err)
	}
}

// A start while this daemon copies the share calls the move off: the copy
// goes, the share is as it was, and the move says it was called off.
func TestShareMoveGateCallsOffAMoveInFlight(t *testing.T) {
	ctx := context.Background()
	r := newMoveRig(t, topology.SingleNode)
	r.mu.Lock()
	// The copy Job's pod never runs: the move waits on it.
	r.podFor = func(job *batchv1.Job) *corev1.Pod { return jobPodOf(job, corev1.PodPending) }
	r.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- r.pool.MoveShare(ctx, testCluster, "alpha", nil) }()
	deadline := time.Now().Add(5 * time.Second)
	for r.pvc(r.copyClaim) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the move made no copy")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := r.pool.ShareMoveGate(ctx, testCluster, "alpha"); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if err := <-done; !errors.Is(err, errShareMoveCalledOff) {
		t.Fatalf("the move ended with %v", err)
	}
	if r.pvc(r.copyClaim) != nil || classOf(r.pvc(r.claim)) != "standard" || r.pvc(r.claim).UID != "share-uid-1" {
		t.Fatal("a called-off move changed the share")
	}
}
