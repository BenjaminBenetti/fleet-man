//go:build !nok8s

package owner

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
)

// Against a real API server: a restored copy of a live daemon is refused the
// Lease, takes it over once the original stops renewing, and two connections
// of one daemon to the namespace hold separate Leases. Set
// FLEET_KUBE_TEST_CONTEXT to a kube context to run it; it creates and
// deletes one fleet-it-p1-01-owner-* namespace.
func TestLiveLeaseConflictAndTakeover(t *testing.T) {
	kctx := os.Getenv("FLEET_KUBE_TEST_CONTEXT")
	if kctx == "" {
		t.Skip("FLEET_KUBE_TEST_CONTEXT is not set")
	}
	ns := "fleet-it-p1-01-owner-" + strings.ToLower(names.NewClusterID("x", nil)[2:])
	conn, err := kubeexec.Load(fleet.KubeCluster{ID: "live-abc234", Context: kctx, KubeconfigPaths: kubeexec.DefaultKubeconfigPaths(os.Environ()), Namespace: ns}, kubeexec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cs, err := conn.Clientset()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	})

	const d = 3 * time.Second
	a := NewLease(cs.CoordinationV1(), ns, "live-abc234", original, LeaseOptions{Duration: d})
	b := NewLease(cs.CoordinationV1(), ns, "live-abc234", restored, LeaseOptions{Duration: d})
	other := NewLease(cs.CoordinationV1(), ns, "other-abc234", original, LeaseOptions{Duration: d})

	if st, err := a.Ensure(ctx); err != nil || !st.Held {
		t.Fatalf("original Ensure = %+v, %v", st, err)
	}
	if st, err := other.Ensure(ctx); err != nil || !st.Held {
		t.Fatalf("second connection Ensure = %+v, %v", st, err)
	}
	// While the original renews, the copy is refused.
	for range 4 {
		st, err := b.Ensure(ctx)
		if err != nil || st.Held || st.Holder != original.Incarnation() {
			t.Fatalf("copy Ensure while the original renews = %+v, %v", st, err)
		}
		time.Sleep(d / 2)
		if st, err := a.Ensure(ctx); err != nil || !st.Held {
			t.Fatalf("original renewal = %+v, %v", st, err)
		}
	}
	// The original stops; the copy takes over after a full duration.
	deadline := time.Now().Add(4 * d)
	for {
		st, err := b.Ensure(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.Held {
			if !st.TookOver {
				t.Errorf("takeover status = %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the copy never took over an abandoned Lease: %+v", st)
		}
		time.Sleep(d / 3)
	}
	if st, err := a.Ensure(ctx); err != nil || st.Held || st.Holder != restored.Incarnation() {
		t.Fatalf("original after the takeover = %+v, %v", st, err)
	}
	lease, err := cs.CoordinationV1().Leases(ns).Get(ctx, b.Name(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Labels[names.LabelCluster] != "live-abc234" || lease.Labels[names.LabelRole] != string(names.RoleOwner) || *lease.Spec.LeaseTransitions != 1 {
		t.Errorf("lease = %+v %+v", lease.Labels, lease.Spec)
	}
	if err := b.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if st, err := other.Ensure(ctx); err != nil || !st.Held {
		t.Errorf("the second connection's Lease after the first's release = %+v, %v", st, err)
	}
}
