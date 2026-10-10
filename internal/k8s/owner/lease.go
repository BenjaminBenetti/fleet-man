//go:build !nok8s

package owner

import (
	"context"
	"fmt"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

const (
	// LeaseDuration is how long an owner Lease stays live after its holder
	// last renewed it.
	LeaseDuration = 90 * time.Second
	// RenewInterval is how often the holder should call Ensure.
	RenewInterval = 30 * time.Second
)

// Lease is one connection's owner Lease (names.OwnerLease, in the
// connection's namespace). It records which incarnation of the daemon may
// adopt and garbage-collect the connection's objects. Call Ensure every
// RenewInterval; it creates, renews or takes over the Lease, or reports the
// other live incarnation that holds it.
//
// Whether another holder is gone is judged on this daemon's own clock: its
// Lease counts as expired once it has gone unchanged for its duration since
// this daemon first saw it so, whatever times it wrote. A skewed clock on
// either side can therefore delay a takeover but never cause one while the
// holder still renews. A daemon that just started waits a full duration
// before taking over from a holder it has not seen renew.
//
// The Lease carries the connection's labels and role=owner, so GC passes,
// which select on the same labels, must leave RoleOwner objects alone.
type Lease struct {
	leases   coordinationv1client.LeaseInterface
	name     string
	holder   string
	host     string
	labels   map[string]string
	duration time.Duration
	now      func() time.Time

	mu sync.Mutex
	// seen is another holder's Lease as last observed and when (local
	// clock), for the expiry judgement.
	seen    string
	seenAt  time.Time
	heldAt  time.Time // when Ensure last found this incarnation holding it
	hasHeld bool
}

// LeaseOptions adjusts a Lease; zero values take the defaults.
type LeaseOptions struct {
	// Duration replaces LeaseDuration (tests).
	Duration time.Duration
	// Now replaces time.Now (tests).
	Now func() time.Time
}

// NewLease returns the owner Lease of the connection clusterID, whose
// objects live in namespace, for this daemon identity.
func NewLease(leases coordinationv1client.LeasesGetter, namespace, clusterID string, id Identity, opts LeaseOptions) *Lease {
	l := &Lease{
		leases:   leases.Leases(namespace),
		name:     names.OwnerLease(id.DaemonID, clusterID),
		holder:   id.Incarnation(),
		host:     id.Host,
		labels:   names.Base(id.Owner(), clusterID),
		duration: opts.Duration,
		now:      opts.Now,
	}
	l.labels[names.LabelRole] = string(names.RoleOwner)
	if l.duration <= 0 {
		l.duration = LeaseDuration
	}
	if l.now == nil {
		l.now = time.Now
	}
	return l
}

// Name is the Lease's object name.
func (l *Lease) Name() string { return l.name }

// LeaseStatus is the outcome of one Ensure.
type LeaseStatus struct {
	// Held: this incarnation holds the Lease now. Otherwise another live
	// incarnation does, and the connection is OWNER_CONFLICT: no adoption
	// and no GC.
	Held bool
	// TookOver: this Ensure took the Lease from an expired holder.
	TookOver bool
	// Holder is the holder's incarnation ("<owner>-<nonce>"), and HolderHost
	// the hostname it reported.
	Holder     string
	HolderHost string
	// RenewedAt is the holder's last renewal, as the holder's clock wrote it.
	RenewedAt time.Time
	// FreeAt, for a Lease held elsewhere, is when this daemon will take it
	// over if the holder renews no more.
	FreeAt time.Time
}

// Ensure creates the Lease, renews it, or takes it over from a holder that
// has expired, and reports who holds it. Run it every RenewInterval.
func (l *Lease) Ensure(ctx context.Context) (LeaseStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for range 4 {
		st, retry, err := l.ensureOnce(ctx)
		if err != nil || !retry {
			if err == nil && st.Held {
				// From before the write: a takeover counts from no earlier
				// than when the other side saw it.
				l.heldAt, l.hasHeld = st.RenewedAt, true
			} else if err == nil {
				l.hasHeld = false
			}
			return st, err
		}
	}
	return LeaseStatus{}, fmt.Errorf("owner lease %s kept changing under concurrent writers", l.name)
}

// ensureOnce is one read-decide-write round; retry reports a lost
// optimistic-concurrency race worth another round.
func (l *Lease) ensureOnce(ctx context.Context) (st LeaseStatus, retry bool, err error) {
	now := l.now()
	cur, err := l.leases.Get(ctx, l.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err := l.leases.Create(ctx, l.claim(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: l.name}}, now, true), metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return LeaseStatus{}, true, nil
		}
		if err != nil {
			return LeaseStatus{}, false, fmt.Errorf("creating owner lease %s: %w", l.name, err)
		}
		return l.heldStatus(now, false), false, nil
	}
	if err != nil {
		return LeaseStatus{}, false, fmt.Errorf("reading owner lease %s: %w", l.name, err)
	}

	holder := ""
	if cur.Spec.HolderIdentity != nil {
		holder = *cur.Spec.HolderIdentity
	}
	if holder == l.holder {
		return l.write(ctx, l.claim(cur.DeepCopy(), now, false), now, false)
	}
	if holder == "" || l.expired(cur, holder, now) {
		lease := l.claim(cur.DeepCopy(), now, true)
		transitions := int32(1)
		if cur.Spec.LeaseTransitions != nil {
			transitions += *cur.Spec.LeaseTransitions
		}
		lease.Spec.LeaseTransitions = &transitions
		return l.write(ctx, lease, now, true)
	}
	st = LeaseStatus{Holder: holder, HolderHost: cur.Annotations[names.AnnotationHolderHost], FreeAt: l.seenAt.Add(leaseDuration(cur, l.duration))}
	if cur.Spec.RenewTime != nil {
		st.RenewedAt = cur.Spec.RenewTime.Time
	}
	return st, false, nil
}

func (l *Lease) write(ctx context.Context, lease *coordinationv1.Lease, now time.Time, tookOver bool) (LeaseStatus, bool, error) {
	_, err := l.leases.Update(ctx, lease, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return LeaseStatus{}, true, nil
	}
	if err != nil {
		return LeaseStatus{}, false, fmt.Errorf("updating owner lease %s: %w", l.name, err)
	}
	l.seen = ""
	return l.heldStatus(now, tookOver), false, nil
}

// expired reports whether another holder's Lease has gone unchanged for its
// duration on this daemon's clock. A renewal changes both the resource
// version and the renew time, whatever the holder's clock says.
func (l *Lease) expired(cur *coordinationv1.Lease, holder string, now time.Time) bool {
	seen := holder + "\x00" + cur.ResourceVersion
	if cur.Spec.RenewTime != nil {
		seen += "\x00" + cur.Spec.RenewTime.UTC().Format(time.RFC3339Nano)
	}
	if seen != l.seen {
		l.seen, l.seenAt = seen, now
		return false
	}
	return !now.Before(l.seenAt.Add(leaseDuration(cur, l.duration)))
}

func leaseDuration(cur *coordinationv1.Lease, def time.Duration) time.Duration {
	if cur.Spec.LeaseDurationSeconds != nil && *cur.Spec.LeaseDurationSeconds > 0 {
		return time.Duration(*cur.Spec.LeaseDurationSeconds) * time.Second
	}
	return def
}

// claim stamps this incarnation onto lease: a renewal, or (acquire) a new
// tenure.
func (l *Lease) claim(lease *coordinationv1.Lease, now time.Time, acquire bool) *coordinationv1.Lease {
	if lease.Labels == nil {
		lease.Labels = map[string]string{}
	}
	for k, v := range l.labels {
		lease.Labels[k] = v
	}
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	lease.Annotations[names.AnnotationHolderHost] = l.host
	t := metav1.NewMicroTime(now)
	seconds := int32(l.duration / time.Second)
	lease.Spec.HolderIdentity = &l.holder
	lease.Spec.LeaseDurationSeconds = &seconds
	lease.Spec.RenewTime = &t
	if acquire {
		lease.Spec.AcquireTime = &t
	}
	return lease
}

func (l *Lease) heldStatus(now time.Time, tookOver bool) LeaseStatus {
	return LeaseStatus{Held: true, TookOver: tookOver, Holder: l.holder, HolderHost: l.host, RenewedAt: now}
}

// Holding reports whether this incarnation may act as the owner right now:
// the last Ensure found it holding the Lease, recently enough that no other
// incarnation can have taken it over since (a takeover needs a full duration
// without renewals; this claims five sixths of one). GC checks it before
// deleting.
func (l *Lease) Holding() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hasHeld && l.now().Before(l.heldAt.Add(l.duration*5/6))
}

// Release deletes the Lease if this incarnation holds it (when its
// connection is removed); a Lease held elsewhere is left alone.
func (l *Lease) Release(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hasHeld = false
	cur, err := l.leases.Get(ctx, l.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading owner lease %s: %w", l.name, err)
	}
	if cur.Spec.HolderIdentity == nil || *cur.Spec.HolderIdentity != l.holder {
		return nil
	}
	err = l.leases.Delete(ctx, l.name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &cur.ResourceVersion}})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("deleting owner lease %s: %w", l.name, err)
	}
	return nil
}
