//go:build !nok8s

package kubernetes

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

func TestElectSpokeIsTheOldestRunningPod(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	got, ok := ElectSpoke([]SpokeCandidate{{"fi-b", t0.Add(time.Minute)}, {"fi-c", t0}, {"fi-a", t0}})
	if !ok || got.StatefulSet != "fi-a" {
		t.Fatalf("ElectSpoke = %+v %v, want the oldest (by name among equals)", got, ok)
	}
	if _, ok := ElectSpoke(nil); ok {
		t.Fatal("ElectSpoke elected a pod of none")
	}
}

// TestLeasesOnlyGoForward: a spoke whose lease epoch is older than one
// fleetd saw on the same tree lost the lease and is refused.
func TestLeasesOnlyGoForward(t *testing.T) {
	tree := "c-" + t.Name() + "/fs-alpha-abcdefgh-share"
	for _, step := range []struct {
		lease uint64
		ok    bool
	}{{3, true}, {3, true}, {4, true}, {2, false}, {5, true}} {
		if err := acceptLease(tree, step.lease); (err == nil) != step.ok {
			t.Fatalf("acceptLease(%d) = %v, want ok=%v", step.lease, err, step.ok)
		}
	}
	if err := acceptLease(tree+"-other", 1); err != nil {
		t.Fatalf("another tree's leases: %v", err)
	}
}

func TestShareSides(t *testing.T) {
	shared := ShareRecord{Mode: "single-node", Claim: "fs-alpha-abcdefgh-share"}
	a, b := ShareSide("c1", "key1aaaa", shared), ShareSide("c1", "key2bbbb", shared)
	if a != b || a.ID != "k8s:c1/fs-alpha-abcdefgh-share" || a.Location != "c1" {
		t.Fatalf("a share's pods are sides %+v and %+v, want one side of the share, located at the connection", a, b)
	}
	copyTier := ShareRecord{Mode: "copy"}
	if a, b := ShareSide("c1", "key1aaaa", copyTier), ShareSide("c1", "key2bbbb", copyTier); a == b || !strings.HasSuffix(a.ID, "key1aaaa") {
		t.Fatalf("copy-tier instances are sides %+v and %+v, want one each", a, b)
	}
}

// TestShareCandidates lists the ready instance pods of a share-group that
// mount the share, as ElectSpoke takes them.
func TestShareCandidates(t *testing.T) {
	claim := "fs-alpha-abcdefgh-share"
	pod := func(name string, ready bool, mounts bool, started time.Time, role names.Role) *corev1.Pod {
		labels := names.Base(handleOwner, handleCluster)
		labels[names.LabelShareGroup] = "abcdefgh"
		labels[names.LabelRole] = string(role)
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: handleNS, Labels: labels}, Status: corev1.PodStatus{StartTime: &metav1.Time{Time: started}}}
		if ready {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		if mounts {
			p.Spec.Volumes = []corev1.Volume{{Name: "share", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}}
		}
		return p
	}
	t0 := time.Unix(1700000000, 0)
	r := newHandleRig(t, []kruntime.Object{
		pod("fi-alpha-web-aaaaaaaa-0", true, true, t0.Add(time.Hour), names.RoleInstance),
		pod("fi-alpha-api-bbbbbbbb-0", true, true, t0, names.RoleInstance),
		pod("fi-alpha-new-cccccccc-0", false, true, t0, names.RoleInstance),
		pod("fi-alpha-old-dddddddd-0", true, false, t0, names.RoleInstance),
		pod("fi-alpha-web-aaaaaaaa-seed-g1-x", true, true, t0, names.RoleSeed),
	}...)
	got, err := r.pool.ShareCandidates(context.Background(), handleCluster, "abcdefgh", claim)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates %+v, want the two ready instance pods mounting the share", got)
	}
	if e, _ := ElectSpoke(got); e.StatefulSet != "fi-alpha-api-bbbbbbbb" {
		t.Fatalf("elected %+v, want the oldest", e)
	}
}
