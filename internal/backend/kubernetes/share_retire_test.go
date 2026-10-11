//go:build !nok8s

package kubernetes

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
)

// TestRetireShare: a destroyed fleet's share is kept labelled an orphan
// when its flush failed, deleted otherwise; a fleet without one there, or a
// PVC of its name that is not this daemon's, is left alone.
func TestRetireShare(t *testing.T) {
	r := newHandleRig(t)
	ident, err := owner.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h8 := names.FleetHash(ident.DaemonID, handleCluster, "alpha")
	claim := ShareClaim("alpha", h8)
	pvcs := r.cs.CoreV1().PersistentVolumeClaims(handleNS)
	if _, err := pvcs.Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: handleNS, Labels: shareLabels(ident.Owner(), handleCluster, h8)}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.pool.RetireShare(ctx, handleCluster, "alpha", true); err != nil || got != claim {
		t.Fatalf("RetireShare(keep) = %q, %v", got, err)
	}
	pvc, err := pvcs.Get(ctx, claim, metav1.GetOptions{})
	if err != nil || pvc.Labels[names.LabelOrphan] != names.OrphanFlushFailed {
		t.Fatalf("the kept share: %v %v", pvc.GetLabels(), err)
	}
	if got, err := r.pool.RetireShare(ctx, handleCluster, "alpha", false); err != nil || got != claim {
		t.Fatalf("RetireShare = %q, %v", got, err)
	}
	if _, err := pvcs.Get(ctx, claim, metav1.GetOptions{}); !gone(err) {
		t.Fatalf("the share was not deleted: %v", err)
	}
	if got, err := r.pool.RetireShare(ctx, handleCluster, "alpha", false); err != nil || got != "" {
		t.Fatalf("RetireShare without a share = %q, %v", got, err)
	}
	foreign := ShareClaim("beta", names.FleetHash(ident.DaemonID, handleCluster, "beta"))
	if _, err := pvcs.Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: foreign, Namespace: handleNS, Labels: map[string]string{names.LabelManagedBy: "someone"}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.pool.RetireShare(ctx, handleCluster, "beta", false); err != nil || got != "" {
		t.Fatalf("RetireShare of a foreign PVC = %q, %v", got, err)
	}
	if _, err := pvcs.Get(ctx, foreign, metav1.GetOptions{}); err != nil {
		t.Fatalf("a foreign PVC was deleted: %v", err)
	}
}

// TestPrepareShareTakesUpAnOrphanedShare: a fleet made again under the name
// of one whose share was kept an orphan takes it up, and it is no orphan any
// more.
func TestPrepareShareTakesUpAnOrphanedShare(t *testing.T) {
	labels := shareLabels(handleOwner, handleCluster, h8())
	labels[names.LabelOrphan] = names.OrphanFlushFailed
	cs := fake.NewClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: ShareClaim("alpha", h8()), Namespace: handleNS, Labels: labels, Annotations: map[string]string{names.AnnotationShareMode: "single-node", names.AnnotationShareTopology: "[]"}},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	})
	c := shareCreation(cs, "web", shareCaps(wffc), fleet.KubeCluster{})
	if err := c.prepareShare(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.share.done()
	pvc, err := cs.CoreV1().PersistentVolumeClaims(handleNS).Get(context.Background(), ShareClaim("alpha", h8()), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, orphan := pvc.Labels[names.LabelOrphan]; orphan || c.share.claim != pvc.Name {
		t.Fatalf("the share taken up: labels %v, claim %q", pvc.Labels, c.share.claim)
	}
}

// TestElectShareSpoke: on the copy tier an instance runs its own spoke; a
// fleet share's runs in its oldest running pod that names one of fleetd's
// records mounting the share, "" when none runs.
func TestElectShareSpoke(t *testing.T) {
	ctx := context.Background()
	copyTier := ShareRecord{Mode: "copy"}
	r := newHandleRig(t)
	if got, err := r.pool.ElectShareSpoke(ctx, handleCluster, "alpha", "aaaaaaaa", copyTier); err != nil || got != "aaaaaaaa" {
		t.Fatalf("copy tier: %q %v", got, err)
	}
	ident, err := owner.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	h8 := names.FleetHash(ident.DaemonID, handleCluster, "alpha")
	claim := ShareClaim("alpha", h8)
	share := ShareRecord{Mode: "single-node", Claim: claim}
	t0 := time.Unix(1700000000, 0)
	pod := func(inst, key string, started time.Time) *corev1.Pod {
		labels := names.Base(handleOwner, handleCluster)
		labels[names.LabelShareGroup] = h8
		labels[names.LabelRole] = string(names.RoleInstance)
		labels[names.LabelKey] = key
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: names.Pod(names.Instance("alpha", inst, key)), Namespace: handleNS, Labels: labels},
			Spec:       corev1.PodSpec{Volumes: []corev1.Volume{{Name: "share", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}}},
			Status:     corev1.PodStatus{StartTime: &metav1.Time{Time: started}, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
		}
	}
	if got, err := r.pool.ElectShareSpoke(ctx, handleCluster, "alpha", "aaaaaaaa", share); err != nil || got != "" {
		t.Fatalf("no pod: %q %v", got, err)
	}
	for _, p := range []*corev1.Pod{pod("web", "aaaaaaaa", t0.Add(time.Hour)), pod("api", "bbbbbbbb", t0), pod("odd", "cccccccc", t0.Add(-time.Hour))} {
		if _, err := r.cs.CoreV1().Pods(handleNS).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// Records for web and api; the oldest pod (odd) names none of fleetd's.
	for _, rec := range []InstanceRecord{
		{V: 1, Cluster: handleCluster, Namespace: handleNS, Key: "aaaaaaaa", Fleet: "alpha", Instance: "web", StatefulSet: names.Instance("alpha", "web", "aaaaaaaa"), Gen: 1, Share: &share},
		{V: 1, Cluster: handleCluster, Namespace: handleNS, Key: "bbbbbbbb", Fleet: "alpha", Instance: "api", StatefulSet: names.Instance("alpha", "api", "bbbbbbbb"), Gen: 1, Share: &share},
	} {
		if err := SaveInstanceRecord(rec); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := r.pool.ElectShareSpoke(ctx, handleCluster, "alpha", "aaaaaaaa", share); err != nil || got != "bbbbbbbb" {
		t.Fatalf("elected %q (%v), want api's, the oldest with a record", got, err)
	}
}
