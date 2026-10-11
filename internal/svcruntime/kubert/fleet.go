//go:build !nok8s

package kubert

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// fleet.go is a fleet's objects on a connection over their life: prepared
// before its first instance there, a service's added when it is on, a cache
// wiped, and all of them torn down when the fleet leaves.

// The kinds of a fleet's objects, in the order a teardown deletes them (the
// anchor, a ConfigMap, after what it owns), and those of one service's
// server (its volume aside).
var (
	fleetKinds  = []string{kubernetes.KindDeployment, kubernetes.KindService, kubernetes.KindNetworkPolicy, kubernetes.KindSecret, kubernetes.KindConfigMap}
	serverKinds = []string{kubernetes.KindDeployment, kubernetes.KindService, kubernetes.KindSecret, kubernetes.KindConfigMap}
)

// fleetLabel selects the fleet's objects (with the connection's own labels,
// which a Scope adds).
func (m fleetMeta) fleetLabel() string { return names.LabelFleet + "=" + m.Hash }

// fleetObjectsSelector selects the objects a fleet teardown deletes.
func (m fleetMeta) fleetObjectsSelector() string {
	return m.fleetLabel() + "," + names.LabelRole + " in (" + strings.Join([]string{
		string(names.RoleAnchor), string(names.RoleNet), string(names.RolePolicy), string(names.RoleCache),
	}, ",") + ")"
}

// serviceSelector selects svc's objects.
func (m fleetMeta) serviceSelector(svc fleet.SharedService) string {
	return m.fleetLabel() + "," + names.LabelRole + "=" + string(names.RoleCache) + "," + names.LabelService + "=" + string(svc)
}

// podSelector selects svc's pods, with the connection's labels.
func (m fleetMeta) podSelector(svc fleet.SharedService) string {
	return names.Selector(m.Owner, m.Cluster) + "," + m.serviceSelector(svc)
}

// ensureAnchor applies the fleet's anchor and returns it as stored.
func (c *conn) ensureAnchor(ctx context.Context, m fleetMeta) (metav1.OwnerReference, error) {
	a, err := c.apply(ctx, anchorConfigMap(m))
	if err != nil {
		return metav1.OwnerReference{}, err
	}
	return anchorRef(a.GetName(), a.GetUID()), nil
}

// prepare applies the anchor, the headless Service and the NetworkPolicies.
func (c *conn) prepare(ctx context.Context, m fleetMeta) error {
	owner, err := c.ensureAnchor(ctx, m)
	if err != nil {
		return err
	}
	objs := []metav1.Object{netService(m, owner), instancesPolicy(m, owner), cachesPolicy(m, owner)}
	block := c.rec.BlocksMetadata()
	if block {
		objs = append(objs, egressPolicy(m, owner))
	}
	for _, o := range objs {
		if _, err := c.apply(ctx, o); err != nil {
			return err
		}
	}
	if !block {
		// The connection stopped blocking metadata: the fleet's egress
		// policy goes, if it is fleet's.
		np := egressPolicy(m, owner)
		ops, _ := opsFor(c.scope.Client, c.scope.Namespace, np)
		live, err := ops.get(ctx, np.Name)
		switch {
		case gone(err):
		case err != nil:
			return fmt.Errorf("reading %s/%s: %w", ops.kind, np.Name, err)
		case ours(live, np) == "":
			if err := c.delete(ctx, ops, np.Name, live.GetUID()); err != nil {
				return fmt.Errorf("removing the metadata block %s/%s: %w", ops.kind, np.Name, err)
			}
		}
	}
	return nil
}

// ensureService applies svc's volume, server and Service (and the anchor).
func (c *conn) ensureService(ctx context.Context, m fleetMeta, svc fleet.SharedService) error {
	owner, err := c.ensureAnchor(ctx, m)
	if err != nil {
		return err
	}
	if err := c.ensureVolume(ctx, m, svc); err != nil {
		return err
	}
	in := deploymentInput{Rec: &c.rec}
	if svc == fleet.ServiceBuildkit {
		// Its config, and its certificate with the CA it takes instances'
		// certificates of; rootful where the connection allows privileged
		// pods, else rootless.
		if _, err := c.apply(ctx, buildkitConfig(m, owner)); err != nil {
			return err
		}
		ca, _, err := ensureCA(c.cluster, m, now())
		if err != nil {
			return err
		}
		if _, err := c.apply(ctx, buildkitSecret(m, owner, ca)); err != nil {
			return err
		}
		in.Rootful, in.TLSHash = c.buildkitRootful(ctx, m), ca.tlsHash()
	}
	dep, err := cacheDeployment(m, svc, owner, in)
	if err != nil {
		return err
	}
	if _, err := c.apply(ctx, dep); err != nil {
		return err
	}
	_, err = c.apply(ctx, cacheService(m, svc, owner))
	return err
}

// ensureVolume applies svc's volume at its configured size, or the size it
// has when that is larger (a volume only grows: an expanded one keeps its
// size, and a smaller setting applies to the next volume, after a wipe). An
// existing volume keeps its StorageClass, which cannot change.
func (c *conn) ensureVolume(ctx context.Context, m fleetMeta, svc fleet.SharedService) error {
	size, err := cacheSize(&c.rec, svc)
	if err != nil {
		return err
	}
	want := cacheVolume(m, &c.rec, svc, size)
	live, err := c.scope.Client.CoreV1().PersistentVolumeClaims(m.Namespace).Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case gone(err):
	case err != nil:
		return fmt.Errorf("reading %s/%s: %w", kubernetes.KindPVC, want.Name, err)
	default:
		have := live.Spec.Resources.Requests[corev1.ResourceStorage]
		if have.Cmp(size) > 0 {
			want.Spec.Resources.Requests[corev1.ResourceStorage] = have.DeepCopy()
		}
		want.Spec.StorageClassName = live.Spec.StorageClassName
	}
	_, err = c.apply(ctx, want)
	if err != nil && live != nil && live.Name != "" && isResizeRefused(err) {
		// A larger setting on a class that cannot grow its volumes: the
		// volume keeps its size, and the cache works on.
		have := live.Spec.Resources.Requests[corev1.ResourceStorage]
		flog.Warn("kube caches: the cache volume cannot grow to its new size; it keeps its size", "cluster", c.cluster, "fleet", m.Fleet, "service", svc, "size", have.String(), "wanted", size.String(), "err", err)
		want.Spec.Resources.Requests[corev1.ResourceStorage] = have.DeepCopy()
		_, err = c.apply(ctx, want)
	}
	return err
}

// isResizeRefused reports an API server refusal to grow a volume.
func isResizeRefused(err error) bool {
	return (apierrors.IsForbidden(err) || apierrors.IsInvalid(err)) && strings.Contains(err.Error(), "resize")
}

// wipe empties svc's cache: the server scaled to nothing, its volume
// deleted and made again, the server brought back and waited for.
func (c *conn) wipe(ctx context.Context, m fleetMeta, svc fleet.SharedService) error {
	deps := c.scope.Client.AppsV1().Deployments(m.Namespace)
	name := m.name(cacheSpecs[svc].suffix)
	if d, err := deps.Get(ctx, name, metav1.GetOptions{}); err == nil {
		if why := ours(d, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Labels: m.serviceLabels(svc)}}); why != "" {
			return &ForeignError{Object: kubernetes.KindDeployment + "/" + name, Why: why}
		}
		patch := []byte(`{"spec":{"replicas":0}}`)
		if _, err := deps.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: kubernetes.DefaultFieldManager}); err != nil && !gone(err) {
			return fmt.Errorf("stopping the server: %w", err)
		}
	} else if !gone(err) {
		return fmt.Errorf("reading %s/%s: %w", kubernetes.KindDeployment, name, err)
	}
	if err := c.waitNoPods(ctx, m, svc); err != nil {
		return err
	}
	if _, err := c.scope.DeleteSelected(ctx, m.serviceSelector(svc), []string{kubernetes.KindPVC}); err != nil {
		return fmt.Errorf("deleting the cache volume: %w", err)
	}
	if err := c.waitSelectedGone(ctx, m.serviceSelector(svc), []string{kubernetes.KindPVC}); err != nil {
		return fmt.Errorf("deleting the cache volume: %w", err)
	}
	flog.Info("kube caches: cache volume deleted", "cluster", c.cluster, "fleet", m.Fleet, "service", svc)
	if svc == fleet.ServiceBuildkit && c.buildkitRefusal(ctx) != nil {
		// Not run on this connection: nothing to bring back, and the
		// stopped server goes.
		c.stopRefusedBuildkit(ctx, m)
		return nil
	}
	if err := c.ensureService(ctx, m, svc); err != nil {
		return err
	}
	setReady(c.cluster, m, svc, stateStarting)
	if err := c.waitAvailable(ctx, m, svc); err != nil {
		setReady(c.cluster, m, svc, "failed: "+err.Error())
		return fmt.Errorf("bringing the server back: %w", err)
	}
	setReady(c.cluster, m, svc, stateReady)
	return nil
}

// waitNoPods waits for svc's pods to be gone (its volume unmounted).
func (c *conn) waitNoPods(ctx context.Context, m fleetMeta, svc fleet.SharedService) error {
	pods := c.scope.Client.CoreV1().Pods(m.Namespace)
	for {
		l, err := pods.List(ctx, metav1.ListOptions{LabelSelector: m.podSelector(svc)})
		if err == nil && len(l.Items) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the server's pod would not stop: %w", context.Cause(ctx))
		case <-time.After(pollInterval):
		}
	}
}

// waitSelectedGone waits until nothing of kinds matches selector.
func (c *conn) waitSelectedGone(ctx context.Context, selector string, kinds []string) error {
	for {
		left, err := c.scope.ListSelected(ctx, selector, kinds)
		if err == nil && len(left) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s still there: %w", strings.Join(left, ", "), context.Cause(ctx))
		case <-time.After(pollInterval):
		}
	}
}

// waitAvailable waits for svc's server to be available, failing early, and
// by name, on what keeps it from starting.
func (c *conn) waitAvailable(ctx context.Context, m fleetMeta, svc fleet.SharedService) error {
	name := m.name(cacheSpecs[svc].suffix)
	deps := c.scope.Client.AppsV1().Deployments(m.Namespace)
	pods := c.scope.Client.CoreV1().Pods(m.Namespace)
	last := "waiting for its pod"
	for {
		d, err := deps.Get(ctx, name, metav1.GetOptions{})
		if err == nil && d.Status.ObservedGeneration >= d.Generation && d.Status.AvailableReplicas >= 1 {
			return nil
		}
		if l, err := pods.List(ctx, metav1.ListOptions{LabelSelector: m.podSelector(svc)}); err == nil {
			for i := range l.Items {
				why, fatal := podTrouble(&l.Items[i])
				if why != "" {
					last = why
				}
				if fatal {
					return errors.New(why)
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not up yet (%s): %w", last, context.Cause(ctx))
		case <-time.After(pollInterval):
		}
	}
}

// podTrouble is what keeps a service pod from running ("" when nothing
// does), and whether it will not get better on its own.
func podTrouble(p *corev1.Pod) (string, bool) {
	for _, cs := range p.Status.ContainerStatuses {
		w := cs.State.Waiting
		if w == nil {
			continue
		}
		switch w.Reason {
		case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
			return "its image does not pull: " + firstLine(w.Message), true
		case "CreateContainerConfigError", "CreateContainerError", "CrashLoopBackOff":
			return w.Reason + ": " + firstLine(w.Message), w.Reason != "CrashLoopBackOff" || cs.RestartCount >= 3
		}
	}
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Message != "" {
			return "not scheduled: " + firstLine(cond.Message), false
		}
	}
	return "", false
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// deleteFleetObjects deletes the fleet's objects on the connection by label
// (its servers, Services and NetworkPolicies, then its anchor, which a
// deletion by hand or by `fleet cluster gc --apply` would also take them
// with), and its cache volumes too with deleteCaches.
func (c *conn) deleteFleetObjects(ctx context.Context, m fleetMeta, deleteCaches bool) (kubernetes.Teardown, error) {
	td, err := c.scope.DeleteSelected(ctx, m.fleetObjectsSelector(), fleetKinds)
	if err != nil {
		return td, err
	}
	if deleteCaches {
		v, err := c.scope.DeleteSelected(ctx, m.fleetLabel()+","+names.LabelRole+"="+string(names.RoleCache), []string{kubernetes.KindPVC})
		td.Deleted, td.Remaining = append(td.Deleted, v.Deleted...), append(td.Remaining, v.Remaining...)
		if err != nil {
			return td, err
		}
	}
	return td, nil
}

// PendingError is a fleet teardown that could not run now: the daemon
// recorded it and finishes it once the connection is usable again.
type PendingError struct {
	Cluster, Fleet string
	Err            error
}

func (e *PendingError) Error() string {
	return fmt.Sprintf("the fleet's objects on connection %s are deleted once it can be reached (%v)", e.Cluster, e.Err)
}

func (e *PendingError) Unwrap() error { return e.Err }

// errUnreachable is a teardown put off because the connection is not usable.
var errUnreachable = errors.New("the cluster is not reachable now")

// teardown deletes fleetName's objects on the connection: on a fleet destroy
// (gone) all of them, its cache volumes too where the connection deletes
// them with the fleet, and the fleet's presence goes; when its last instance
// there goes (!gone) all but its cache volumes, unless a record of the fleet
// names the connection again. What the cluster cannot take now becomes a
// pending teardown (footprint.json) and a PendingError.
func (r *Runtime) teardown(fleetName string, gone bool) error {
	unlock := r.lock(fleetName)
	defer unlock()
	if !gone && fleetUsesCluster(fleetName, r.cluster) {
		return nil
	}
	// The fleet left the connection: its shared Buildkit there takes no
	// certificate of its instances' again, whether its objects go now or
	// later.
	r.rotate(fleetName)
	c, err := r.connect()
	if errors.Is(err, kubernetes.ErrClusterNotFound) {
		// The connection was removed: nothing can reach what is left.
		_ = kubernetes.SettleFleetTeardown(r.cluster, fleetName, true)
		return nil
	}
	pending := kubernetes.PendingTeardown{FleetGone: gone}
	if err != nil {
		// The connection's record still says whether its cache volumes go
		// with the fleet, when its client does not load now.
		if rec, rerr := pool().Record(r.cluster); rerr == nil {
			pending.DeleteCaches = gone && rec.DeleteCacheVolumesOnDestroy
		}
		return r.pend(fleetName, "", pending, err)
	}
	m := c.meta(fleetName)
	pending.DeleteCaches = gone && c.rec.DeleteCacheVolumesOnDestroy
	if !usable(r.cluster) {
		return r.pend(fleetName, m.Hash, pending, errUnreachable)
	}
	ctx, cancel := opContext()
	defer cancel()
	td, err := c.deleteFleetObjects(ctx, m, pending.DeleteCaches)
	if err != nil {
		return r.pend(fleetName, m.Hash, pending, err)
	}
	forgetFleetReady(r.cluster, m)
	if err := kubernetes.SettleFleetTeardown(r.cluster, fleetName, gone); err != nil {
		flog.Warn("kube caches: recording a fleet teardown", "cluster", r.cluster, "fleet", fleetName, "err", err)
	}
	flog.Info("kube caches: the fleet's objects on the connection are deleted", "cluster", r.cluster, "fleet", fleetName,
		"fleet_destroyed", gone, "cache_volumes_deleted", pending.DeleteCaches, "deleted", len(td.Deleted), "still_going", len(td.Remaining))
	return nil
}

// pend records a teardown the connection could not take and returns it as a
// PendingError.
func (r *Runtime) pend(fleetName, hash string, t kubernetes.PendingTeardown, cause error) error {
	if hash == "" {
		if id, err := daemonID(); err == nil {
			hash = names.FleetHash(id, r.cluster, fleetName)
		}
	}
	t.Error = cause.Error()
	if err := kubernetes.RecordPendingTeardown(r.cluster, fleetName, hash, t); err != nil {
		flog.Warn("kube caches: recording a pending fleet teardown", "cluster", r.cluster, "fleet", fleetName, "err", err)
	}
	flog.Warn("kube caches: the fleet's objects on the connection are deleted later", "cluster", r.cluster, "fleet", fleetName, "err", cause)
	return &PendingError{Cluster: r.cluster, Fleet: fleetName, Err: cause}
}

// RunPendingTeardowns finishes the fleet teardowns owed on connection
// clusterID (the reconciler's background pass, while the connection is
// usable and the daemon holds its owner Lease) and reports how many are
// still owed.
func RunPendingTeardowns(ctx context.Context, clusterID string) int {
	fp, err := kubernetes.LoadFootprint()
	if err != nil {
		flog.Warn("kube caches: reading the footprint", "err", err)
		return 0
	}
	left := 0
	for _, p := range fp.PresencesOf(clusterID) {
		if p.PendingTeardown == nil {
			continue
		}
		if ctx.Err() != nil {
			left++
			continue
		}
		if err := For(clusterID).finishPending(ctx, p); err != nil {
			left++
			flog.Warn("kube caches: a pending fleet teardown failed again", "cluster", clusterID, "fleet", p.Fleet, "err", err)
		}
	}
	return left
}

// finishPending runs p's pending teardown, unless the fleet came back to the
// connection meanwhile (its next PrepareFleet then makes what it needs).
func (r *Runtime) finishPending(ctx context.Context, p kubernetes.FleetPresence) error {
	unlock := r.lock(p.Fleet)
	defer unlock()
	t := p.PendingTeardown
	if fleetUsesCluster(p.Fleet, r.cluster) {
		return kubernetes.SettleFleetTeardown(r.cluster, p.Fleet, false)
	}
	r.rotate(p.Fleet)
	c, err := r.connect()
	if err != nil {
		return err
	}
	m := c.meta(p.Fleet)
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	td, err := c.deleteFleetObjects(ctx, m, t.DeleteCaches)
	if err != nil {
		_ = kubernetes.RecordPendingTeardown(r.cluster, p.Fleet, m.Hash, kubernetes.PendingTeardown{DeleteCaches: t.DeleteCaches, FleetGone: t.FleetGone, Error: err.Error()})
		return err
	}
	forgetFleetReady(r.cluster, m)
	gone := t.FleetGone || !fleetExists(p.Fleet)
	flog.Info("kube caches: finished a pending fleet teardown", "cluster", r.cluster, "fleet", p.Fleet, "deleted", len(td.Deleted), "owed_since", t.Since)
	return kubernetes.SettleFleetTeardown(r.cluster, p.Fleet, gone)
}

// PurgeConnection deletes every object of the fleets recorded on connection
// clusterID, their cache volumes included, as the connection is
// disconnected: no later connection reaches them (a connection added again
// is another, with other names). A fleet a record still names there is left
// alone. It returns a line for each fleet whose objects could not all go.
func PurgeConnection(ctx context.Context, clusterID string) []string {
	fp, err := kubernetes.LoadFootprint()
	if err != nil {
		return []string{"reading the footprint: " + err.Error()}
	}
	var failed []string
	for _, p := range fp.PresencesOf(clusterID) {
		if err := For(clusterID).purge(ctx, p.Fleet); err != nil {
			failed = append(failed, fmt.Sprintf("fleet %s: %v", p.Fleet, err))
		}
	}
	return failed
}

func (r *Runtime) purge(ctx context.Context, fleetName string) error {
	unlock := r.lock(fleetName)
	defer unlock()
	if fleetUsesCluster(fleetName, r.cluster) {
		return nil
	}
	r.rotate(fleetName)
	c, err := r.connect()
	if err != nil {
		return err
	}
	m := c.meta(fleetName)
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	td, err := c.deleteFleetObjects(ctx, m, true)
	if err != nil {
		return err
	}
	forgetFleetReady(r.cluster, m)
	flog.Info("kube caches: deleted a fleet's objects on a connection being removed", "cluster", r.cluster, "fleet", fleetName, "deleted", len(td.Deleted))
	return kubernetes.SettleFleetTeardown(r.cluster, fleetName, true)
}

// rotate forgets fleetName's Buildkit CA on the connection (tls.go), under
// the fleet's lock.
func (r *Runtime) rotate(fleetName string) {
	id, err := daemonID()
	if err != nil {
		flog.Warn("kube buildkit: the daemon's identity, to rotate a fleet's CA", "cluster", r.cluster, "fleet", fleetName, "err", err)
		return
	}
	removed, err := rotateCA(r.cluster, names.FleetHash(id, r.cluster, fleetName))
	switch {
	case err != nil:
		flog.Warn("kube buildkit: rotating the fleet's CA", "cluster", r.cluster, "fleet", fleetName, "err", err)
	case removed:
		flog.Info("kube buildkit: the fleet's CA on the connection is rotated: no certificate it signed is taken there again", "cluster", r.cluster, "fleet", fleetName)
	}
}

// PendingTeardowns counts the fleet teardowns owed on connection clusterID.
func PendingTeardowns(clusterID string) int {
	fp, err := kubernetes.LoadFootprint()
	if err != nil {
		return 0
	}
	n := 0
	for _, p := range fp.PresencesOf(clusterID) {
		if p.PendingTeardown != nil {
			n++
		}
	}
	return n
}

// PresentClusters are the connections the daemon recorded fleetName's
// objects on.
func PresentClusters(fleetName string) []string {
	fp, err := kubernetes.LoadFootprint()
	if err != nil {
		return nil
	}
	return fp.ClustersOf(fleetName)
}

// growQuantity is q grown by half, rounded up to a whole Gi.
func growQuantity(q resource.Quantity) resource.Quantity {
	const gi = int64(1) << 30
	n := q.Value() + q.Value()/2
	n = (n + gi - 1) / gi * gi
	return *resource.NewQuantity(n, resource.BinarySI)
}
