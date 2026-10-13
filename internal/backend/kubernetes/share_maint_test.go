//go:build !nok8s

package kubernetes

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
)

// TestMaintainShare: a share 90% full grows by half where its class
// expands; one that cannot grow carries the warning its Storage section
// shows; one that is not measured (no pod of the fleet has a session) is
// left alone.
func TestMaintainShare(t *testing.T) {
	for _, tc := range []struct {
		name      string
		used      int64
		measured  bool
		expands   bool
		wantSize  string
		wantWarn  string
		wantUsage int
	}{
		{"nearly full, expandable", 9500 << 20, true, true, "15Gi", "", 92},
		{"nearly full, fixed", 9500 << 20, true, false, "10Gi", "92% full; its StorageClass cannot grow it", 92},
		{"roomy", 1 << 30, true, true, "10Gi", "", 10},
		{"not measured", 0, false, true, "10Gi", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHandleRig(t)
			ident, err := owner.LoadIdentity()
			if err != nil {
				t.Fatal(err)
			}
			h8 := names.FleetHash(ident.DaemonID, handleCluster, "alpha")
			claim := ShareClaim("alpha", h8)
			class := "standard"
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: handleNS, Labels: shareLabels(ident.Owner(), handleCluster, h8)},
				Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &class, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}},
				Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
			}
			if _, err := r.cs.CoreV1().PersistentVolumeClaims(handleNS).Create(context.Background(), pvc, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			SetEnvironment(Environment{Facts: func(context.Context, string) (Facts, error) {
				return Facts{Capabilities: &fleetgrpc.KubeCapabilities{StorageClasses: []*fleetgrpc.KubeStorageClass{{Name: "standard", AllowExpansion: tc.expands}}}}, nil
			}})
			t.Cleanup(func() { SetEnvironment(Environment{}) })
			prev := shareMeasure
			shareMeasure = func(*Pool, context.Context, string, string, string) (int64, bool) { return tc.used, tc.measured }
			t.Cleanup(func() { shareMeasure = prev })

			u, ok, err := r.pool.MaintainShare(context.Background(), handleCluster, "alpha")
			if err != nil || !ok {
				t.Fatalf("MaintainShare = %v, %v", ok, err)
			}
			if u.Percent() != tc.wantUsage || u.Claim != claim || u.CapacityBytes != 10<<30 || u.Expandable != tc.expands || !strings.HasPrefix(u.Warning, tc.wantWarn) || (tc.wantWarn == "") != (u.Warning == "") {
				t.Fatalf("usage %+v (%d%%)", u, u.Percent())
			}
			got, _ := r.cs.CoreV1().PersistentVolumeClaims(handleNS).Get(context.Background(), claim, metav1.GetOptions{})
			if size := got.Spec.Resources.Requests[corev1.ResourceStorage]; size.String() != tc.wantSize {
				t.Fatalf("the share asks for %s, want %s", size.String(), tc.wantSize)
			}
		})
	}
}

// A fleet without a share on the connection has no usage.
func TestShareUsageWithoutAShare(t *testing.T) {
	r := newHandleRig(t)
	if _, ok, err := r.pool.ShareUsage(context.Background(), handleCluster, "alpha"); ok || err != nil {
		t.Fatalf("ShareUsage = %v, %v", ok, err)
	}
}

// TestMeasureShareSkipsAPodWithoutAKey: a pod with fleet's labels but no
// instance key (one some other hand made, its name too short to hold one)
// is never measured in, nor does it stop the upkeep.
func TestMeasureShareSkipsAPodWithoutAKey(t *testing.T) {
	claim := "fs-alpha-abcdefgh-share"
	labels := names.Base(handleOwner, handleCluster)
	labels[names.LabelShareGroup] = "abcdefgh"
	labels[names.LabelRole] = string(names.RoleInstance)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "x-0", Namespace: handleNS, Labels: labels},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "share", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
		}}}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	r := newHandleRig(t, pod)
	if used, ok := r.pool.measureShare(context.Background(), handleCluster, "abcdefgh", claim); ok || used != 0 {
		t.Fatalf("measureShare = %d, %v, want nothing measured", used, ok)
	}
}

// A growth is under way until the filesystem has it: while the volume is not
// its requested size, and while the node has still to grow the filesystem.
func TestResizeUnderway(t *testing.T) {
	claim := func(request, capacity string, conds ...corev1.PersistentVolumeClaimConditionType) *corev1.PersistentVolumeClaim {
		pvc := &corev1.PersistentVolumeClaim{}
		pvc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(request)}
		if capacity != "" {
			pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)}
		}
		for _, c := range conds {
			pvc.Status.Conditions = append(pvc.Status.Conditions, corev1.PersistentVolumeClaimCondition{Type: c, Status: corev1.ConditionTrue})
		}
		return pvc
	}
	for _, tc := range []struct {
		name string
		pvc  *corev1.PersistentVolumeClaim
		want bool
	}{
		{"settled", claim("2Gi", "2Gi"), false},
		{"not bound yet", claim("2Gi", ""), false},
		{"volume still growing", claim("2Gi", "1Gi"), true},
		{"resizing", claim("2Gi", "2Gi", corev1.PersistentVolumeClaimResizing), true},
		{"filesystem still to grow", claim("2Gi", "2Gi", corev1.PersistentVolumeClaimFileSystemResizePending), true},
	} {
		if got := ResizeUnderway(tc.pvc); got != tc.want {
			t.Errorf("%s: ResizeUnderway = %v, want %v", tc.name, got, tc.want)
		}
	}
}
