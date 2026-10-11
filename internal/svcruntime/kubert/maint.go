//go:build !nok8s

package kubert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// maint.go is the upkeep of a fleet's servers on a connection: whether each
// is up (watched after every ensure, without making the ensure wait), how
// full its volume is, growing a nearly full one where its StorageClass
// allows, and apt-cacher-ng's daily expiry run, which its image has no cron
// for.

// The states CacheUsage reports for a server besides "failed: <why>".
const (
	stateStarting = "starting"
	stateReady    = "ready"
)

// aptMaintEvery is how often the deb cache's expiry runs.
var aptMaintEvery = 24 * time.Hour

var (
	readyMu sync.Mutex
	// ready is each server's last known state, by readyKey.
	ready = map[string]string{}
	// watching are the servers watchReady follows now.
	watching = map[string]bool{}
	// aptMaintAt is when each deb cache last ran its expiry.
	aptMaintAt = map[string]time.Time{}
	// warned are the volumes whose 90% warning fleet.log has had.
	warned = map[string]bool{}
)

func readyKey(cluster string, m fleetMeta, svc fleet.SharedService) string {
	return cluster + "/" + m.Hash + "/" + string(svc)
}

func setReady(cluster string, m fleetMeta, svc fleet.SharedService, state string) {
	readyMu.Lock()
	ready[readyKey(cluster, m, svc)] = state
	readyMu.Unlock()
}

func readyState(cluster string, m fleetMeta, svc fleet.SharedService) string {
	readyMu.Lock()
	defer readyMu.Unlock()
	return ready[readyKey(cluster, m, svc)]
}

func forgetReady(cluster string, m fleetMeta, svc fleet.SharedService) {
	readyMu.Lock()
	delete(ready, readyKey(cluster, m, svc))
	readyMu.Unlock()
}

func forgetFleetReady(cluster string, m fleetMeta) {
	for _, svc := range fleet.SharedServices() {
		forgetReady(cluster, m, svc)
	}
}

// watchReady follows svc's server coming up in the background (at most one
// watch per server), recording its state for CacheUsage and fleet.log: an
// ensure never waits for the server, whose first image pull can take
// minutes. A server already known up is not watched again.
func watchReady(cluster string, m fleetMeta, svc fleet.SharedService) {
	key := readyKey(cluster, m, svc)
	readyMu.Lock()
	if watching[key] || ready[key] == stateReady {
		readyMu.Unlock()
		return
	}
	watching[key] = true
	if ready[key] == "" {
		ready[key] = stateStarting
	}
	readyMu.Unlock()
	go func() {
		defer func() {
			readyMu.Lock()
			delete(watching, key)
			readyMu.Unlock()
		}()
		r := For(cluster)
		c, err := r.connect()
		if err != nil {
			setReady(cluster, m, svc, "failed: "+err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
		defer cancel()
		start := now()
		if err := c.waitAvailable(ctx, m, svc); err != nil {
			setReady(cluster, m, svc, "failed: "+err.Error())
			flog.Warn("kube caches: the server is not up", "cluster", cluster, "fleet", m.Fleet, "service", svc, "err", err)
			return
		}
		setReady(cluster, m, svc, stateReady)
		flog.Info("kube caches: the server is up", "cluster", cluster, "fleet", m.Fleet, "service", svc, "ms", now().Sub(start).Milliseconds())
	}()
}

// CacheUsage reports each of fleetName's cache volumes on the connection:
// its use (measured in its server's pod) against its size, whether it grows,
// its server's state, and the warning its Caching row shows at 90% full when
// it cannot grow.
func (r *Runtime) CacheUsage(ctx context.Context, fleetName string) ([]fleet.CacheUsage, error) {
	c, err := r.connect()
	if err != nil {
		return nil, err
	}
	m := c.meta(fleetName)
	var out []fleet.CacheUsage
	for _, svc := range fleet.SharedServices() {
		u, ok, err := c.usage(ctx, m, svc)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, u)
		}
	}
	return out, nil
}

// usage is svc's volume's usage; ok is false when the fleet has no such
// volume on the connection.
func (c *conn) usage(ctx context.Context, m fleetMeta, svc fleet.SharedService) (fleet.CacheUsage, bool, error) {
	s := cacheSpecs[svc]
	pvc, err := c.scope.Client.CoreV1().PersistentVolumeClaims(m.Namespace).Get(ctx, m.name(s.suffix), metav1.GetOptions{})
	if gone(err) {
		return fleet.CacheUsage{}, false, nil
	}
	if err != nil {
		return fleet.CacheUsage{}, false, err
	}
	if ours(pvc, cacheVolume(m, &c.rec, svc, pvc.Spec.Resources.Requests[corev1.ResourceStorage])) != "" {
		return fleet.CacheUsage{}, false, nil
	}
	u := fleet.CacheUsage{Service: svc, Cluster: c.cluster, Location: c.rec.GetDisplayName()}
	capacity := pvc.Status.Capacity[corev1.ResourceStorage]
	if capacity.IsZero() {
		capacity = pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	}
	u.CapacityBytes = capacity.Value()
	u.Expandable = c.rec.AutoExpandsDisk() && c.classExpands(ctx, pvc)
	u.State = readyState(c.cluster, m, svc)
	if u.State == "" {
		u.State = c.serverState(ctx, m, svc)
	}
	if used, err := c.measure(ctx, m, svc); err == nil {
		u.UsedBytes = used
	} else if u.State == stateReady {
		u.State = "ready (its use could not be read: " + firstLine(err.Error()) + ")"
	}
	if u.Percent() >= fleet.CacheFullPercent && !u.Expandable {
		u.Warning = fmt.Sprintf("%d%% full; ", u.Percent())
		if c.rec.AutoExpandsDisk() {
			u.Warning += "its StorageClass cannot grow it"
		} else {
			u.Warning += "auto-expand is off on " + c.rec.GetDisplayName()
		}
	}
	return u, true, nil
}

// serverState is svc's server's state as the cluster shows it now.
func (c *conn) serverState(ctx context.Context, m fleetMeta, svc fleet.SharedService) string {
	d, err := c.scope.Client.AppsV1().Deployments(m.Namespace).Get(ctx, m.name(cacheSpecs[svc].suffix), metav1.GetOptions{})
	switch {
	case gone(err):
		return "stopped"
	case err != nil:
		return "unknown"
	case d.Status.AvailableReplicas >= 1:
		return stateReady
	}
	return stateStarting
}

// classExpands reports whether pvc's StorageClass grows its volumes, as the
// connection's last preflight found it (or, without one, the class itself
// says).
func (c *conn) classExpands(ctx context.Context, pvc *corev1.PersistentVolumeClaim) bool {
	class := ""
	if pvc.Spec.StorageClassName != nil {
		class = *pvc.Spec.StorageClassName
	}
	if f, err := connectionFacts(ctx, c.cluster); err == nil && f.Capabilities != nil {
		for _, sc := range f.Capabilities.GetStorageClasses() {
			if sc.GetName() == class || (class == "" && sc.GetIsDefault()) {
				return sc.GetAllowExpansion()
			}
		}
	}
	if class == "" {
		return false
	}
	sc, err := c.scope.Client.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{})
	return err == nil && sc.AllowVolumeExpansion != nil && *sc.AllowVolumeExpansion
}

// servicePod is a running pod of svc's server.
func (c *conn) servicePod(ctx context.Context, m fleetMeta, svc fleet.SharedService) (string, error) {
	l, err := c.scope.Client.CoreV1().Pods(m.Namespace).List(ctx, metav1.ListOptions{LabelSelector: m.podSelector(svc)})
	if err != nil {
		return "", err
	}
	for _, p := range l.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("the %s server has no running pod", serviceText(svc))
}

// measure is how many bytes svc's cache holds: du in its server's pod (the
// size of the cache itself, which on a node-local volume is not the
// filesystem's use).
func (c *conn) measure(ctx context.Context, m fleetMeta, svc fleet.SharedService) (int64, error) {
	pod, err := c.servicePod(ctx, m, svc)
	if err != nil {
		return 0, err
	}
	var stdout, stderr bytes.Buffer
	code, err := pool().ExecService(ctx, c.cluster, pod, string(svc), []string{"du", "-sxk", cacheSpecs[svc].data}, &stdout, &stderr)
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 0, fmt.Errorf("du exited %d: %s", code, firstLine(stderr.String()))
	}
	return parseDu(stdout.String())
}

// parseDu reads `du -sk`'s kilobytes as bytes.
func parseDu(out string) (int64, error) {
	f := strings.Fields(out)
	if len(f) == 0 {
		return 0, fmt.Errorf("du printed nothing")
	}
	kb, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("du printed %q", firstLine(out))
	}
	return kb * 1024, nil
}

// Maintain is the upkeep of fleetName's servers on the connection for the
// services it has on: a volume 90% full grows by half where its StorageClass
// allows and the connection auto-expands (else fleet.log warns, and its
// Caching row says so), and the deb cache runs its expiry once a day.
func (r *Runtime) Maintain(ctx context.Context, fleetName string, services []fleet.SharedService) error {
	c, err := r.connect()
	if err != nil {
		return err
	}
	m := c.meta(fleetName)
	for _, svc := range services {
		if svc == fleet.ServiceBuildkit {
			continue // not served on a cluster yet
		}
		u, ok, err := c.usage(ctx, m, svc)
		if err != nil || !ok {
			continue
		}
		key := readyKey(c.cluster, m, svc)
		switch {
		case u.Percent() >= fleet.CacheFullPercent && u.Expandable:
			if err := r.grow(ctx, c, m, svc); err != nil {
				flog.Warn("kube caches: growing a nearly full cache volume", "cluster", c.cluster, "fleet", fleetName, "service", svc, "err", err)
			}
		case u.Warning != "":
			readyMu.Lock()
			first := !warned[key]
			warned[key] = true
			readyMu.Unlock()
			if first {
				flog.Warn("kube caches: a cache volume is nearly full", "cluster", c.cluster, "fleet", fleetName, "service", svc, "warning", u.Warning)
			}
		default:
			readyMu.Lock()
			delete(warned, key)
			readyMu.Unlock()
		}
		if svc == fleet.ServiceDebCache && u.State == stateReady {
			c.aptMaint(ctx, m)
		}
	}
	return nil
}

// grow grows svc's volume by half (to a whole Gi).
func (r *Runtime) grow(ctx context.Context, c *conn, m fleetMeta, svc fleet.SharedService) error {
	unlock := r.lock(m.Fleet)
	defer unlock()
	pvcs := c.scope.Client.CoreV1().PersistentVolumeClaims(m.Namespace)
	name := m.name(cacheSpecs[svc].suffix)
	pvc, err := pvcs.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	have := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok && capacity.Cmp(have) < 0 {
		return nil // a growth asked for earlier has not happened yet
	}
	next := growQuantity(have)
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"resources": map[string]any{"requests": map[string]string{"storage": next.String()}}}})
	if _, err := pvcs.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: kubernetes.DefaultFieldManager}); err != nil {
		return err
	}
	flog.Info("kube caches: grew a nearly full cache volume", "cluster", c.cluster, "fleet", m.Fleet, "service", svc, "from", have.String(), "to", next.String())
	return nil
}

// aptMaint runs the deb cache's expiry (acngtool maint, what its package's
// daily cron job runs) at most once per aptMaintEvery: it drops what no
// index lists any more, which keeps the cache below its volume as packages
// are superseded.
func (c *conn) aptMaint(ctx context.Context, m fleetMeta) {
	key := readyKey(c.cluster, m, fleet.ServiceDebCache)
	readyMu.Lock()
	due := now().Sub(aptMaintAt[key]) >= aptMaintEvery
	if due {
		aptMaintAt[key] = now()
	}
	readyMu.Unlock()
	if !due {
		return
	}
	pod, err := c.servicePod(ctx, m, fleet.ServiceDebCache)
	if err != nil {
		return
	}
	var out bytes.Buffer
	code, err := pool().ExecService(ctx, c.cluster, pod, string(fleet.ServiceDebCache), aptMaintArgv, &out, &out)
	if err != nil || code != 0 {
		flog.Warn("kube caches: the deb cache's expiry run failed", "cluster", c.cluster, "fleet", m.Fleet, "code", code, "err", err, "output", firstLine(out.String()))
		return
	}
	flog.Info("kube caches: the deb cache ran its expiry", "cluster", c.cluster, "fleet", m.Fleet)
}

// aptMaintArgv runs apt-cacher-ng's daily job, or its tool when the image
// has no cron script.
var aptMaintArgv = []string{"sh", "-c", "if [ -x /etc/cron.daily/apt-cacher-ng ]; then exec /etc/cron.daily/apt-cacher-ng; fi; exec /usr/lib/apt-cacher-ng/acngtool maint -c /etc/apt-cacher-ng"}
