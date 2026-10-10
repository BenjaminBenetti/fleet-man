//go:build !nok8s

package owner

import (
	"context"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

const (
	testNS      = "fleet"
	testCluster = "minikube-abc234"
)

// clock is a settable time source shared by a test's daemons.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// The original daemon and a restored copy of its ~/.fleet on another machine:
// one daemon id, two incarnations.
var (
	original = Identity{DaemonID: "0123456789abcdef0123456789abcdef", Nonce: "aaaaaaaaaaaaaaaa", Host: "laptop"}
	restored = Identity{DaemonID: "0123456789abcdef0123456789abcdef", Nonce: "bbbbbbbbbbbbbbbb", Host: "desktop"}
)

func newTestLease(cs *fake.Clientset, id Identity, now func() time.Time) *Lease {
	return NewLease(cs.CoordinationV1(), testNS, testCluster, id, LeaseOptions{Now: now})
}

func ensure(t *testing.T, l *Lease) LeaseStatus {
	t.Helper()
	st, err := l.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return st
}

func getLease(t *testing.T, cs *fake.Clientset) *coordinationv1.Lease {
	t.Helper()
	l, err := cs.CoordinationV1().Leases(testNS).Get(context.Background(), names.OwnerLease(original.DaemonID, testCluster), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get lease: %v", err)
	}
	return l
}

func TestLeaseCreatesAndRenews(t *testing.T) {
	cs, clk := fake.NewClientset(), newClock()
	l := newTestLease(cs, original, clk.now)
	st := ensure(t, l)
	if !st.Held || st.TookOver || st.Holder != original.Incarnation() {
		t.Fatalf("first Ensure = %+v, want held", st)
	}
	lease := getLease(t, cs)
	if lease.Name != "fleet-owner-0123456789ab-"+testCluster {
		t.Errorf("lease name = %q", lease.Name)
	}
	for k, v := range map[string]string{
		names.LabelManagedBy: names.ManagedBy,
		names.LabelOwner:     "0123456789ab",
		names.LabelCluster:   testCluster,
		names.LabelRole:      string(names.RoleOwner),
	} {
		if lease.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, lease.Labels[k], v)
		}
	}
	if lease.Annotations[names.AnnotationHolderHost] != "laptop" || *lease.Spec.LeaseDurationSeconds != 90 {
		t.Errorf("lease = %+v", lease)
	}
	acquired := lease.Spec.AcquireTime.Time

	clk.advance(RenewInterval)
	if st := ensure(t, l); !st.Held {
		t.Fatalf("renewal = %+v", st)
	}
	lease = getLease(t, cs)
	if !lease.Spec.RenewTime.Time.Equal(clk.now()) || !lease.Spec.AcquireTime.Time.Equal(acquired) || lease.Spec.LeaseTransitions != nil {
		t.Errorf("renewal changed the tenure: %+v", lease.Spec)
	}
	if !l.Holding() {
		t.Error("Holding is false while renewing")
	}
}

// A restored daemon_id meets the live original: the copy is refused
// (OWNER_CONFLICT) for as long as the original renews, however long that is
// and whatever its clock says.
func TestLeaseTwoIncarnationsConflict(t *testing.T) {
	cs, clk := fake.NewClientset(), newClock()
	a := newTestLease(cs, original, clk.now)
	ensure(t, a)

	// The copy's clock runs two hours ahead: the original's renew times look
	// ancient to it, which must not matter.
	skewed := func() time.Time { return clk.now().Add(2 * time.Hour) }
	b := newTestLease(cs, restored, skewed)
	for range 10 { // 5 minutes of polling
		st := ensure(t, b)
		if st.Held {
			t.Fatalf("the copy took the Lease from a live holder: %+v", st)
		}
		if st.Holder != original.Incarnation() || st.HolderHost != "laptop" {
			t.Fatalf("conflict status = %+v", st)
		}
		if b.Holding() {
			t.Fatal("the copy reports Holding: GC would run")
		}
		clk.advance(RenewInterval)
		ensure(t, a)
	}
	if !a.Holding() {
		t.Error("the original lost Holding")
	}
}

// Once the original stops renewing, the copy takes over a full duration
// after it first saw the Lease unchanged, and the original then sees the
// conflict.
func TestLeaseExpiredAllowsTakeover(t *testing.T) {
	cs, clk := fake.NewClientset(), newClock()
	a := newTestLease(cs, original, clk.now)
	ensure(t, a)

	b := newTestLease(cs, restored, clk.now)
	first := ensure(t, b)
	if first.Held || !first.FreeAt.Equal(clk.now().Add(LeaseDuration)) {
		t.Fatalf("first look = %+v, want a conflict free at +90s", first)
	}
	clk.advance(LeaseDuration - time.Second)
	if st := ensure(t, b); st.Held {
		t.Fatalf("took over before the duration passed: %+v", st)
	}
	clk.advance(time.Second)
	st := ensure(t, b)
	if !st.Held || !st.TookOver || st.Holder != restored.Incarnation() {
		t.Fatalf("after the duration = %+v, want a takeover", st)
	}
	lease := getLease(t, cs)
	if *lease.Spec.HolderIdentity != restored.Incarnation() || lease.Spec.LeaseTransitions == nil || *lease.Spec.LeaseTransitions != 1 ||
		lease.Annotations[names.AnnotationHolderHost] != "desktop" || !lease.Spec.AcquireTime.Time.Equal(clk.now()) {
		t.Errorf("after takeover lease = %+v %+v", lease.Annotations, lease.Spec)
	}

	// The original comes back: it is now the one in conflict.
	if st := ensure(t, a); st.Held || st.Holder != restored.Incarnation() {
		t.Fatalf("the original after the takeover = %+v", st)
	}
	if a.Holding() {
		t.Error("the original still reports Holding")
	}
}

// A restart on the same machine (same incarnation) renews at once.
func TestLeaseSameIncarnationAfterRestart(t *testing.T) {
	cs, clk := fake.NewClientset(), newClock()
	ensure(t, newTestLease(cs, original, clk.now))
	clk.advance(10 * time.Minute) // long after it expired
	st := ensure(t, newTestLease(cs, original, clk.now))
	if !st.Held || st.TookOver {
		t.Fatalf("restart = %+v, want a plain renewal", st)
	}
}

// Two connections of one daemon to the same namespace have separate Leases.
func TestLeasePerConnection(t *testing.T) {
	cs, clk := fake.NewClientset(), newClock()
	one := NewLease(cs.CoordinationV1(), testNS, "one-abc234", original, LeaseOptions{Now: clk.now})
	two := NewLease(cs.CoordinationV1(), testNS, "two-abc234", original, LeaseOptions{Now: clk.now})
	if one.Name() == two.Name() {
		t.Fatalf("both connections use %s", one.Name())
	}
	if !ensure(t, one).Held || !ensure(t, two).Held {
		t.Fatal("a connection could not hold its own Lease")
	}
	list, err := cs.CoordinationV1().Leases(testNS).List(context.Background(), metav1.ListOptions{})
	if err != nil || len(list.Items) != 2 {
		t.Fatalf("leases = %d (%v), want 2", len(list.Items), err)
	}
}

// Holding lapses when renewals stop succeeding, before anyone else could
// take over.
func TestLeaseHoldingLapses(t *testing.T) {
	cs, clk := fake.NewClientset(), newClock()
	l := newTestLease(cs, original, clk.now)
	ensure(t, l)
	cs.PrependReactor("get", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver down")
	})
	clk.advance(RenewInterval)
	if _, err := l.Ensure(context.Background()); err == nil {
		t.Fatal("Ensure succeeded against a failing apiserver")
	}
	if !l.Holding() {
		t.Error("one failed renewal already dropped Holding")
	}
	clk.advance(LeaseDuration - RenewInterval - RenewInterval/2)
	if l.Holding() {
		t.Error("Holding outlived the safe window after renewals failed")
	}
}

// A lost update race is retried, not reported.
func TestLeaseRetriesConflicts(t *testing.T) {
	cs, clk := fake.NewClientset(), newClock()
	l := newTestLease(cs, original, clk.now)
	ensure(t, l)
	failed := false
	cs.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failed {
			return false, nil, nil
		}
		failed = true
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, l.Name(), nil)
	})
	clk.advance(RenewInterval)
	if st := ensure(t, l); !st.Held || !failed {
		t.Fatalf("Ensure after a conflict = %+v (conflict injected: %v)", st, failed)
	}
}

func TestLeaseRelease(t *testing.T) {
	cs, clk := fake.NewClientset(), newClock()
	a := newTestLease(cs, original, clk.now)
	b := newTestLease(cs, restored, clk.now)
	ensure(t, a)
	if err := b.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	getLease(t, cs) // still there: b never held it
	if err := a.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoordinationV1().Leases(testNS).Get(context.Background(), a.Name(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("after Release: %v, want not found", err)
	}
	if a.Holding() {
		t.Error("Holding after Release")
	}
	if err := a.Release(context.Background()); err != nil {
		t.Errorf("a second Release: %v", err)
	}
}
