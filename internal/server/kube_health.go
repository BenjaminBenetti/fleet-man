//go:build !nok8s

package server

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_health.go keeps the daemon's Kubernetes connections alive and judged:
// one worker per KubeCluster record that runs a full preflight when the
// connection is added, edited or tested and every 30 minutes, a shallow probe
// (the API server's version and the owner Lease renewal) every 30 seconds,
// and pushes the result as Watch Event.kube_clusters. All of it runs in the
// background once fleetd serves (run), so an unreachable cluster or a hung
// login plugin never delays a Hello: the RPCs answer from the last result,
// CHECKING until the first one returns.

// Probe cadence; variables so tests can shorten them.
var (
	kubeFullInterval = 30 * time.Minute
	// kubeProbeInterval is the shallow probe's, which rides on the owner
	// Lease renewal (owner.RenewInterval).
	kubeProbeInterval = owner.RenewInterval
	// kubeRetryInterval replaces it while the connection is failing.
	kubeRetryInterval = 60 * time.Second
	// kubeExpiryWarning is how far ahead a credential's expiry is warned of.
	kubeExpiryWarning = 30 * time.Minute
	// kubeCredentialRecheck paces re-reading a refreshable credential (a
	// plugin run) once its last known expiry has passed.
	kubeCredentialRecheck = 5 * time.Minute
	kubeShallowTimeout    = 20 * time.Second
	kubeReleaseTimeout    = 10 * time.Second
	// kubeVerifiedEvery paces the verified_at writes to kube_clusters.json.
	kubeVerifiedEvery = 5 * time.Minute
)

// kubeLink is a connection's live client side: what kubeexec.Load and its
// clientset give, as the probes and RPCs use it. Tests build one over a fake
// clientset.
type kubeLink struct {
	client     kubernetes.Interface
	raw        func(ctx context.Context, path string) ([]byte, error)
	credential func(ctx context.Context) (kubeexec.Credential, error)
	login      func(ctx context.Context, onCode func(kubeexec.DeviceCode)) (kubeexec.Credential, error)
	auth       kubeexec.AuthKind
	plugin     string // the exec plugin's command
	server     string
	source     string // the kubeconfig file the context comes from
}

// connectKube loads a record and builds its client. It reads only local
// files and runs nothing: the credential plugin runs on the first request.
func connectKube(rec fleet.KubeCluster, environ []string) (*kubeLink, error) {
	conn, err := kubeexec.Load(rec, kubeexec.Options{Environ: environ})
	if err != nil {
		return nil, err
	}
	cs, err := conn.Clientset()
	if err != nil {
		return nil, fmt.Errorf("%w: building the client: %v", kubeexec.ErrMisconfigured, err)
	}
	link := &kubeLink{
		client: cs, raw: preflight.RawGetter(cs), credential: conn.Credential, login: conn.Login,
		auth: conn.Auth, server: conn.Server, source: contextSource(rec),
	}
	if conn.Plugin != nil {
		link.plugin = conn.Plugin.Command
	}
	return link, nil
}

// contextSource is the kubeconfig file that defines rec's context (the first
// pinned file that does), for messages.
func contextSource(rec fleet.KubeCluster) string {
	contexts, err := kubeexec.ListContexts(rec.KubeconfigPaths)
	if err == nil {
		for _, c := range contexts {
			if c.Name == rec.Context {
				return displayPath(c.Source)
			}
		}
	}
	if len(rec.KubeconfigPaths) > 0 {
		return displayPath(rec.KubeconfigPaths[0])
	}
	return ""
}

// displayPath shortens a path under the daemon user's home to ~/...
func displayPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return p
}

// kubeLease is a connection's owner Lease (owner.Lease).
type kubeLease interface {
	Ensure(ctx context.Context) (owner.LeaseStatus, error)
	Holding() bool
	Release(ctx context.Context) error
}

// noLease stands in when the daemon's identity cannot be read: every claim
// fails with why.
type noLease struct{ err error }

func (l noLease) Ensure(context.Context) (owner.LeaseStatus, error) {
	return owner.LeaseStatus{}, l.err
}
func (noLease) Holding() bool                 { return false }
func (noLease) Release(context.Context) error { return nil }

// kubeManager owns the connections at run time.
type kubeManager struct {
	svc *service

	// Seams (production defaults; tests swap them).
	connect      func(rec fleet.KubeCluster, environ []string) (*kubeLink, error)
	environ      func() []string
	now          func() time.Time
	loadIdentity func() (owner.Identity, error)
	newLease     func(link *kubeLink, rec fleet.KubeCluster, id owner.Identity) kubeLease
	loadRegistry func() (*state.KubeClusters, error)
	// configurePool hands the daemon's pool the connections
	// (configureKubePool).
	configurePool func([]fleet.KubeCluster)
	// storageCache opens a connection's storage test cache (nil runs no
	// multi-node storage test).
	storageCache func(id string) *preflight.StorageCache

	idOnce sync.Once
	id     owner.Identity
	idErr  error

	// syncMu makes a sync's registry read and its application one step, so
	// a sync that read the file before a removal can never bring the removed
	// connection's worker (and its Lease) back after the removal's own sync.
	syncMu sync.Mutex

	mu sync.Mutex
	// ctx is the serve loop's; nil until run starts the workers.
	ctx     context.Context
	loaded  bool
	records []fleet.KubeCluster // registry order
	workers map[string]*kubeWorker
	// images is the daemon's image delivery (kube_image.go); nil until
	// Serve starts it, so a connection judged without it skips the image
	// delivery check. pullPush and registrySettle are its pending pushes,
	// registryChangedAt when the gateway's image registry last changed.
	images            *kubeImages
	pullPush          *time.Timer
	registrySettle    *time.Timer
	registryChangedAt time.Time
	// imageGC paces each connection's image collection (kube_backend.go).
	imageGC kubeImageGC
}

func newKubeManager(svc *service) *kubeManager {
	return &kubeManager{
		svc:          svc,
		connect:      connectKube,
		environ:      os.Environ,
		now:          time.Now,
		loadIdentity: owner.LoadIdentity,
		newLease: func(link *kubeLink, rec fleet.KubeCluster, id owner.Identity) kubeLease {
			return owner.NewLease(link.client.CoordinationV1(), rec.Namespace, rec.ID, id, owner.LeaseOptions{})
		},
		loadRegistry:  state.LoadKubeClusters,
		configurePool: configureKubePool,
		storageCache: func(id string) *preflight.StorageCache {
			return preflight.OpenStorageCache(state.KubeStorageTestsPath(id))
		},
		workers: map[string]*kubeWorker{},
	}
}

// identity is the daemon's identity, read once (a local file).
func (m *kubeManager) identity() (owner.Identity, error) {
	m.idOnce.Do(func() {
		m.id, m.idErr = m.loadIdentity()
		if m.idErr != nil {
			flog.Error("kube: daemon identity unavailable", "err", m.idErr)
		}
	})
	return m.id, m.idErr
}

// ownerLabel is this daemon's owner label value, or "" while its identity is
// unreadable.
func (m *kubeManager) ownerLabel() string {
	if id, err := m.identity(); err == nil {
		return id.Owner()
	}
	return ""
}

// run starts a worker per connection and keeps them until ctx ends. The
// serve loop calls it once fleetd is serving.
func (m *kubeManager) run(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	_, _ = m.identity()
	m.sync()
	// A daemon that died mid-build or mid-hook left its docker shim
	// directories behind, the capture's environment values in them.
	go sweepKubeShimDirs(m.now())
	<-ctx.Done()
	m.mu.Lock()
	workers := m.workers
	m.workers = map[string]*kubeWorker{}
	m.mu.Unlock()
	for _, w := range workers {
		w.stop()
	}
}

// sync reloads the registry and converges the workers on it: new records
// get a worker, changed ones a fresh full preflight, removed ones lose
// theirs (and their Lease). The pool learns the set at once too, so a create
// on a connection added a moment ago finds it. Then the set is pushed.
func (m *kubeManager) sync() {
	m.syncMu.Lock()
	reg, err := m.loadRegistry()
	if err != nil {
		m.syncMu.Unlock()
		flog.Error("kube: load connections", "err", err)
		return
	}
	m.configurePool(reg.Clusters)
	m.mu.Lock()
	m.records, m.loaded = reg.Clusters, true
	var stopped []*kubeWorker
	if m.ctx != nil {
		seen := map[string]bool{}
		for _, rec := range reg.Clusters {
			seen[rec.ID] = true
			if w := m.workers[rec.ID]; w != nil {
				w.setRecord(rec)
				continue
			}
			w := m.newWorker(rec)
			m.workers[rec.ID] = w
			w.start(m.ctx)
		}
		for id, w := range m.workers {
			if !seen[id] {
				delete(m.workers, id)
				stopped = append(stopped, w)
			}
		}
	}
	m.mu.Unlock()
	m.syncMu.Unlock()
	for _, w := range stopped {
		w.stop()
		go w.releaseLease()
	}
	m.publish()
}

func (m *kubeManager) newWorker(rec fleet.KubeCluster) *kubeWorker {
	return &kubeWorker{m: m, rec: rec, wake: make(chan struct{}, 1), fullWanted: true}
}

// worker returns the running worker of a connection, or nil.
func (m *kubeManager) worker(id string) *kubeWorker {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workers[id]
}

// record returns the stored record of a connection, read from
// kube_clusters.json (not the workers' copy), so a read-modify-write starts
// from what is on disk.
func (m *kubeManager) record(id string) (fleet.KubeCluster, bool) {
	reg, err := state.LoadKubeClusters()
	if err != nil {
		flog.Warn("kube: load connections", "err", err)
		return fleet.KubeCluster{}, false
	}
	if rec := reg.Find(id); rec != nil {
		return *rec, true
	}
	return fleet.KubeCluster{}, false
}

func (m *kubeManager) ensureLoaded() {
	m.mu.Lock()
	loaded := m.loaded
	m.mu.Unlock()
	if !loaded {
		m.sync()
	}
}

// statuses is every connection with its health, in registry order.
func (m *kubeManager) statuses() []*fleetgrpc.KubeClusterStatus {
	m.ensureLoaded()
	m.mu.Lock()
	records := append([]fleet.KubeCluster(nil), m.records...)
	workers := make(map[string]*kubeWorker, len(m.workers))
	for id, w := range m.workers {
		workers[id] = w
	}
	m.mu.Unlock()
	out := make([]*fleetgrpc.KubeClusterStatus, 0, len(records))
	for _, rec := range records {
		if w := workers[rec.ID]; w != nil {
			out = append(out, w.status())
			continue
		}
		out = append(out, checkingStatus(rec))
	}
	return out
}

// status is one connection's current status.
func (m *kubeManager) status(id string) *fleetgrpc.KubeClusterStatus {
	if w := m.worker(id); w != nil {
		return w.status()
	}
	if rec, ok := m.record(id); ok {
		return checkingStatus(rec)
	}
	return nil
}

func checkingStatus(rec fleet.KubeCluster) *fleetgrpc.KubeClusterStatus {
	return &fleetgrpc.KubeClusterStatus{
		Cluster: protoconv.KubeClusterToProto(rec),
		State:   fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING,
		Message: "checking…",
	}
}

// publish pushes the current set over Watch. The hub drops a push equal to
// the last one.
func (m *kubeManager) publish() {
	if m.svc == nil || m.svc.hub == nil {
		return
	}
	snapshot := &fleetgrpc.KubeClusters{Clusters: m.statuses()}
	m.svc.hub.post(func(h *hub) { h.broadcastKubeClusters(snapshot) })
}

// persistBookkeeping records what a probe learned about a connection (its
// API server, its cluster's id, a passing preflight) unless the record was
// re-pointed meanwhile. A recorded cluster id is replaced only with
// replaceUID (the connection's context now reaches another cluster and no
// instance was made on the old one). Client-facing revision and updated_at
// stay as they are: these are the daemon's own notes.
func (m *kubeManager) persistBookkeeping(rec fleet.KubeCluster, server, clusterUID string, replaceUID bool, verified time.Time) {
	key := kubeConnectionKey(rec)
	changed := false
	err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		cur := k.Find(rec.ID)
		if cur == nil || kubeConnectionKey(*cur) != key {
			return errUnchanged
		}
		if server != "" && cur.Server != server {
			cur.Server, changed = server, true
		}
		if clusterUID != "" && cur.ClusterUID != clusterUID && (cur.ClusterUID == "" || replaceUID) {
			cur.ClusterUID, changed = clusterUID, true
		}
		if !verified.IsZero() && verified.Sub(cur.VerifiedAt) >= kubeVerifiedEvery {
			cur.VerifiedAt, changed = verified, true
		}
		if !changed {
			return errUnchanged
		}
		return nil
	})
	if err != nil && !errors.Is(err, errUnchanged) {
		flog.Warn("kube: record connection bookkeeping", "cluster", rec.ID, "err", err)
		return
	}
	if changed {
		m.sync()
	}
}

// errUnchanged aborts a registry update that has nothing to write.
var errUnchanged = errors.New("unchanged")

// kubeConnectionKey is what decides which cluster and objects a record
// reaches: changing any of it re-points the connection.
func kubeConnectionKey(rec fleet.KubeCluster) string {
	return strings.Join([]string{rec.Context, strings.Join(rec.KubeconfigPaths, "\x00"), rec.Namespace,
		strings.Join(rec.ExtraPath, "\x00"), strings.Join(rec.PluginEnv, "\x00")}, "\x01")
}

// --- one connection ---

// kubeWorker probes one connection.
type kubeWorker struct {
	m *kubeManager

	wake chan struct{}
	// cancel is set by start.
	cancel context.CancelFunc
	// claimMu orders releasing retired Leases before making a new one.
	claimMu sync.Mutex

	mu  sync.Mutex
	rec fleet.KubeCluster
	// link is the client for linkKey; nil until the next probe loads it.
	link    *kubeLink
	linkKey string
	lease   kubeLease
	// retired are the Leases of the targets a re-point left, released before
	// the next one is claimed: on the same cluster and namespace they are the
	// same Lease object, and a release that landed after the new claim's
	// renewal would delete the Lease the new one thinks it holds.
	retired []kubeLease
	// fullWanted asks for a full preflight at the next turn.
	fullWanted bool

	// The last full preflight, when it started, and when it returned;
	// deepAt is when the last deep test finished.
	haveFull    bool
	full        preflight.Result
	fullStarted time.Time
	lastFull    time.Time
	deepAt      time.Time
	// multiPodRWO is what the last deep test found of two pods on one node
	// mounting one RWO volume (nil before one told): full runs without a
	// deep test keep it (it decides the connection's share mode), as its
	// storage test cache does across restarts.
	multiPodRWO *bool
	// storage is the connection's storage test cache, opened on first use.
	storage *preflight.StorageCache
	// lastProbe is when the last probe of either kind returned.
	lastProbe time.Time
	// probeFail is the failure the last shallow probe found since the last
	// full one (UNSPECIFIED when it found none).
	probeFail    fleetgrpc.KubeClusterState
	probeFailMsg string
	// reachFailed: the last full run failed where a shallow probe looks too
	// (loading, credentials, reach), so a shallow probe that passes means it
	// recovered. A failure only a full run sees (a missing namespace, RBAC)
	// waits for the next full run, an edit or a test.
	reachFailed bool
	leaseHeld   bool
	// cred is the credential's last known expiry, credErr why re-reading it
	// failed, credAt when it was read.
	cred    kubeexec.Credential
	credErr error
	credAt  time.Time
	// stuck names a probe that has not returned for far longer than it may.
	stuck string
}

func (w *kubeWorker) start(ctx context.Context) {
	ctx, w.cancel = context.WithCancel(ctx)
	go w.loop(ctx)
}

func (w *kubeWorker) stop() {
	if w.cancel != nil {
		w.cancel()
	}
}

func (w *kubeWorker) poke() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// setRecord takes a changed record: a re-pointed one drops its client (and
// its Lease, released by the next probe before it claims the new target's);
// any change asks for a full run.
func (w *kubeWorker) setRecord(rec fleet.KubeCluster) {
	w.mu.Lock()
	old := w.rec
	w.rec = rec
	if kubeConnectionKey(old) != kubeConnectionKey(rec) {
		w.link, w.linkKey = nil, ""
		if w.lease != nil {
			w.retired = append(w.retired, w.lease)
		}
		w.lease = nil
		w.haveFull, w.probeFail = false, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNSPECIFIED
	}
	changed := !kubeRecordsEqual(old, rec)
	if changed {
		w.fullWanted = true
	}
	w.mu.Unlock()
	if changed {
		w.poke()
	}
}

// releaseRetired releases the Leases re-points left behind, in order, before
// the worker claims another.
func (w *kubeWorker) releaseRetired() {
	w.mu.Lock()
	retired, id := w.retired, w.rec.ID
	w.retired = nil
	w.mu.Unlock()
	for _, l := range retired {
		releaseLease(l, id)
	}
}

// kubeRecordsEqual compares records as clients see them, ignoring the
// daemon's bookkeeping.
func kubeRecordsEqual(a, b fleet.KubeCluster) bool {
	a.Server, a.ClusterUID, a.VerifiedAt = "", "", time.Time{}
	b.Server, b.ClusterUID, b.VerifiedAt = "", "", time.Time{}
	return proto.Equal(protoconv.KubeClusterToProto(a), protoconv.KubeClusterToProto(b))
}

func (w *kubeWorker) releaseLease() {
	w.releaseRetired()
	w.mu.Lock()
	l := w.lease
	w.lease = nil
	id := w.rec.ID
	w.mu.Unlock()
	if l != nil {
		releaseLease(l, id)
	}
}

func releaseLease(l kubeLease, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReleaseTimeout)
	defer cancel()
	if err := l.Release(ctx); err != nil {
		flog.Warn("kube: release owner lease", "cluster", id, "err", err)
	}
}

func (w *kubeWorker) loop(ctx context.Context) {
	for {
		w.mu.Lock()
		full, wait := w.nextLocked(w.m.now())
		w.mu.Unlock()
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-w.wake:
				timer.Stop()
				continue
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return
		}
		if full {
			w.runFull(ctx, nil)
		} else {
			w.runShallow(ctx)
		}
	}
}

// nextLocked decides the next probe: whether it is a full one, and how long
// to wait for it.
func (w *kubeWorker) nextLocked(now time.Time) (full bool, wait time.Duration) {
	if w.fullWanted || !w.haveFull || now.Sub(w.lastFull) >= kubeFullInterval {
		return true, 0
	}
	every := kubeProbeInterval
	if preflight.Failed(w.currentStateLocked(now)) {
		every = jitter(kubeRetryInterval)
	}
	wait = w.lastProbe.Add(every).Sub(now)
	if untilFull := w.lastFull.Add(kubeFullInterval).Sub(now); untilFull < wait {
		return true, untilFull
	}
	// Wake to republish when a credential's warning or expiry comes due.
	if exp := w.cred.ExpiresAt; !exp.IsZero() && !w.cred.Refreshable {
		for _, at := range []time.Time{exp.Add(-kubeExpiryWarning), exp} {
			if d := at.Sub(now); d > 0 && d < wait {
				wait = d
			}
		}
	}
	return false, wait
}

// jitter spreads d by ±10%.
func jitter(d time.Duration) time.Duration {
	return d + time.Duration((rand.Float64()*0.2-0.1)*float64(d))
}

// ensureLink returns the client for the current record, loading it when the
// record changed or (reload) on every full run, so a rewritten kubeconfig is
// read again. The owner Lease stays with the connection it was made for.
func (w *kubeWorker) ensureLink(reload bool) (*kubeLink, kubeLease, fleet.KubeCluster, error) {
	w.mu.Lock()
	rec, link, key := w.rec, w.link, w.linkKey
	w.mu.Unlock()
	if link != nil && !reload && key == kubeConnectionKey(rec) {
		w.mu.Lock()
		lease := w.lease
		w.mu.Unlock()
		return link, lease, rec, nil
	}
	// The Leases a re-point retired are gone before a new one is made, and
	// a concurrent probe (a test beside the loop) waits for that too.
	w.claimMu.Lock()
	defer w.claimMu.Unlock()
	w.releaseRetired()
	link, err := w.m.connect(rec, w.m.environ())
	if err != nil {
		return nil, nil, rec, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if kubeConnectionKey(w.rec) != kubeConnectionKey(rec) {
		return nil, nil, rec, errors.New("the connection changed while it was being loaded")
	}
	w.link, w.linkKey = link, kubeConnectionKey(rec)
	if w.lease == nil {
		if id, err := w.m.identity(); err != nil {
			w.lease = noLease{err: fmt.Errorf("daemon identity: %w", err)}
		} else {
			w.lease = w.m.newLease(link, rec, id)
		}
	}
	return link, w.lease, rec, nil
}

func (w *kubeWorker) target(rec fleet.KubeCluster, link *kubeLink, lease kubeLease) preflight.Target {
	t := preflight.Target{
		Cluster: rec, Client: link.client, Raw: link.raw, Credential: link.credential,
		Auth: link.auth, Plugin: link.plugin, Source: link.source,
		Owner: w.m.ownerLabel(), Now: w.m.now,
		// A draft (the context picker's test) has no id yet.
		Delivery: w.m.delivery(rec, link, rec.ID != ""),
	}
	w.mu.Lock()
	t.MultiPodRWO = w.multiPodRWO
	// A draft (no id) keeps nothing: it tests no StorageClass.
	if rec.ID != "" && w.m.storageCache != nil {
		if w.storage == nil {
			w.storage = w.m.storageCache(rec.ID)
		}
		t.StorageCache = w.storage
	}
	w.mu.Unlock()
	if lease != nil {
		t.Lease = lease
	}
	return t
}

// runFull runs a full preflight and makes it the connection's health,
// unless one that started later already did. report receives each check.
func (w *kubeWorker) runFull(ctx context.Context, report func(*fleetgrpc.KubeCheck)) preflight.Result {
	return w.runFullDeep(ctx, report, fullRun{})
}

// fullPreflight runs a full preflight (preflight.Full; tests watch what it
// is asked).
var fullPreflight = preflight.Full

// fullRun is what a full run does besides its checks.
type fullRun struct {
	// deep: the deep test's probes too.
	deep bool
	// storage are StorageClasses to run the multi-node test on now
	// (preflight.Target.TestStorage).
	storage []string
}

// runFullDeep is runFull, with what o asks.
func (w *kubeWorker) runFullDeep(ctx context.Context, report func(*fleetgrpc.KubeCheck), o fullRun) preflight.Result {
	deep := o.deep
	started := w.m.now()
	w.mu.Lock()
	wanted := w.fullWanted
	w.fullWanted = false
	w.mu.Unlock()
	bound := preflight.FullTimeout + time.Minute
	if deep {
		bound += preflight.DeepTimeout
	}
	done := w.watchdog("full preflight", bound)
	link, lease, rec, err := w.ensureLink(true)
	var res preflight.Result
	if err != nil {
		res = preflight.LoadFailure(err, w.m.now())
		if report != nil {
			for _, c := range res.Checks {
				report(c)
			}
		}
	} else {
		tgt := w.target(rec, link, lease)
		tgt.Deep, tgt.TestStorage = deep, o.storage
		res = fullPreflight(ctx, tgt, report)
		w.countOrphans(ctx, rec, link, &res)
	}
	done()
	if ctx.Err() != nil && err == nil {
		// Shutdown, or the caller gave up (a test whose client went away):
		// not a verdict, so the run an edit asked for is still owed.
		if wanted {
			w.mu.Lock()
			w.fullWanted = true
			w.mu.Unlock()
			w.poke()
		}
		return res
	}
	w.applyFull(rec, link, res, started)
	return res
}

// countOrphans fills the capabilities' orphan counts: the objects a prune
// would list. Best effort; a working connection only.
func (w *kubeWorker) countOrphans(ctx context.Context, rec fleet.KubeCluster, link *kubeLink, res *preflight.Result) {
	if !preflight.Usable(res.State) || res.Capabilities == nil {
		return
	}
	id, err := w.m.identity()
	if err != nil {
		return
	}
	st, err := state.Load()
	if err != nil {
		return
	}
	octx, cancel := context.WithTimeout(ctx, kubeShallowTimeout)
	defer cancel()
	if objects, err := listPruneObjects(octx, link.client, rec, id, st, "", w.m.now()); err == nil {
		res.Capabilities.Orphans = kubeOrphans(objects)
	}
}

func (w *kubeWorker) applyFull(rec fleet.KubeCluster, link *kubeLink, res preflight.Result, started time.Time) {
	// The context reaches a cluster other than the one recorded (recreated
	// behind the same context, or the kubeconfig rewritten). A connection
	// without instances simply takes the new id, or a later re-point would
	// be checked against a cluster that is gone; one with instances keeps the
	// id they were made under and says they are not there.
	replaceUID := false
	if uid := res.ClusterUID; link != nil && uid != "" && rec.ClusterUID != "" && uid != rec.ClusterUID {
		if w.m.instancesOn(rec.ID) {
			res.Warnings = append(append([]string(nil), res.Warnings...), fmt.Sprintf(
				"context %s now reaches another cluster (id %s, the connection's is %s): its instances are not there", rec.Context, shortClusterUID(uid), shortClusterUID(rec.ClusterUID)))
		} else {
			replaceUID = true
			flog.Info("kube: the connection reaches another cluster now", "cluster", rec.ID, "was", rec.ClusterUID, "now", uid)
		}
	}
	w.mu.Lock()
	if started.Before(w.fullStarted) || kubeConnectionKey(w.rec) != kubeConnectionKey(rec) {
		w.mu.Unlock()
		return // a newer run, or a re-pointed record, already superseded it
	}
	w.haveFull, w.full, w.fullStarted = true, res, started
	w.lastFull, w.lastProbe = res.CheckedAt, res.CheckedAt
	if !res.DeepCheckedAt.IsZero() {
		w.deepAt = res.DeepCheckedAt
	}
	if res.MultiPodRWO != nil {
		w.multiPodRWO = res.MultiPodRWO
	}
	w.probeFail, w.probeFailMsg = fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNSPECIFIED, ""
	w.cred, w.credErr, w.credAt = res.Credential, nil, res.CheckedAt
	w.leaseHeld = res.Lease != nil && res.Lease.Held
	w.reachFailed = link == nil || failedBeforeReach(res)
	leaseHeld := w.leaseHeld
	w.mu.Unlock()
	w.m.publish()
	// The REACHABLE edge: the reconciler's undo jobs and background pass
	// for this connection go (kube_reconcile.go).
	if link != nil && preflight.Usable(res.State) && leaseHeld && rec.ID != "" && w.m.svc != nil && w.m.svc.kubeRecovery != nil {
		w.m.svc.kubeRecovery.reachable(rec.ID)
	}
	// Images no instance holds any more go once past their retention, only
	// while this daemon owns the connection.
	if link != nil && preflight.Usable(res.State) && leaseHeld {
		w.m.collectImages(rec, link)
	}
	if link != nil {
		var verified time.Time
		if preflight.Usable(res.State) {
			verified = res.CheckedAt
		}
		w.m.persistBookkeeping(rec, link.server, res.ClusterUID, replaceUID, verified)
	}
}

// instancesOn reports whether instance records name a connection (agents
// make nothing until they fire); an unreadable state.json counts as yes.
func (m *kubeManager) instancesOn(id string) bool {
	if m.svc == nil {
		return true
	}
	users, err := m.svc.kubeUsers(id)
	return err != nil || hasInstances(users)
}

func shortClusterUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// runShallow probes reach and renews the Lease between full runs. A probe
// that finds the connection healthy again, or the Lease changed hands, asks
// for a full run.
func (w *kubeWorker) runShallow(ctx context.Context) {
	// A connection whose kubeconfig failed to load has no client yet:
	// ensureLink tries again.
	link, lease, rec, err := w.ensureLink(false)
	if err != nil {
		w.applyShallowFailure(preflight.LoadFailure(err, w.m.now()))
		return
	}
	done := w.watchdog("shallow probe", kubeShallowTimeout+time.Minute)
	pctx, cancel := context.WithTimeout(ctx, kubeShallowTimeout)
	res := preflight.Shallow(pctx, w.target(rec, link, lease))
	cancel()
	done()
	if ctx.Err() != nil {
		return
	}
	if preflight.Failed(res.State) {
		w.applyShallowFailure(res)
		return
	}
	held := res.Lease != nil && res.Lease.Held
	w.mu.Lock()
	recovered := w.probeFail != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNSPECIFIED || w.reachFailed
	leaseChanged := res.Lease != nil && held != w.leaseHeld
	w.lastProbe = res.CheckedAt
	w.probeFail, w.probeFailMsg = fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNSPECIFIED, ""
	recheck := w.cred.Refreshable && !w.cred.ExpiresAt.IsZero() && !w.m.now().Before(w.cred.ExpiresAt) &&
		w.m.now().Sub(w.credAt) >= kubeCredentialRecheck
	if recovered || leaseChanged {
		w.fullWanted = true
	}
	w.mu.Unlock()
	if recheck {
		w.recheckCredential(ctx, link)
	}
	w.m.publish()
}

// failedBeforeReach: a full run stopped at its credentials or reach check.
func failedBeforeReach(res preflight.Result) bool {
	for _, c := range res.Checks {
		if c.GetResult() == fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL {
			switch c.GetName() {
			case preflight.CheckKubeconfig, preflight.CheckCredentials, preflight.CheckReachable:
				return true
			}
		}
	}
	return false
}

func (w *kubeWorker) applyShallowFailure(res preflight.Result) {
	w.mu.Lock()
	w.lastProbe = res.CheckedAt
	w.probeFail, w.probeFailMsg = res.State, res.Message
	w.mu.Unlock()
	w.m.publish()
}

// recheckCredential re-reads a refreshable credential whose expiry passed:
// a plugin run that fails now is a login that lapsed.
func (w *kubeWorker) recheckCredential(ctx context.Context, link *kubeLink) {
	if link.credential == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, preflight.CredentialTimeout)
	defer cancel()
	cred, err := link.credential(cctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.credAt = w.m.now()
	if err != nil {
		w.credErr = err
		return
	}
	w.cred, w.credErr = cred, nil
}

// watchdog reports a probe that runs far past its bound (client-go runs a
// credential plugin with no deadline of its own) until it returns.
func (w *kubeWorker) watchdog(what string, after time.Duration) func() {
	timer := time.AfterFunc(after, func() {
		w.mu.Lock()
		w.stuck = what
		id := w.rec.ID
		w.mu.Unlock()
		flog.Warn("kube: probe still running", "cluster", id, "probe", what, "after", after)
		w.m.publish()
	})
	return func() {
		timer.Stop()
		w.mu.Lock()
		w.stuck = ""
		w.mu.Unlock()
	}
}

// currentStateLocked is the state the worker would publish now.
func (w *kubeWorker) currentStateLocked(now time.Time) fleetgrpc.KubeClusterState {
	st, _ := w.stateLocked(now)
	return st
}

func (w *kubeWorker) stateLocked(now time.Time) (fleetgrpc.KubeClusterState, string) {
	switch {
	case w.stuck != "":
		return fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE, "the " + w.stuck + " has not returned (a hung credential plugin?)"
	case !w.haveFull:
		return fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING, "checking…"
	case w.probeFail != fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNSPECIFIED:
		return w.probeFail, w.probeFailMsg
	}
	state, msg := w.full.State, w.full.Message
	if !preflight.Usable(state) {
		return state, msg
	}
	if w.credErr != nil {
		_, m := preflight.Classify(w.credErr)
		return fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED, "the credential could not be refreshed: " + m
	}
	if exp := w.cred.ExpiresAt; !exp.IsZero() && !w.cred.Refreshable && !now.Before(exp) {
		return fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED, "credentials expired " + preflight.Ago(now.Sub(exp)) + " ago"
	}
	return state, msg
}

// status is the connection's status as clients see it now.
func (w *kubeWorker) status() *fleetgrpc.KubeClusterStatus {
	now := w.m.now()
	// Read before w.mu: the manager's lock is taken before a worker's.
	im := w.m.imagesOrNil()
	w.mu.Lock()
	defer w.mu.Unlock()
	st := &fleetgrpc.KubeClusterStatus{Cluster: protoconv.KubeClusterToProto(w.rec)}
	st.State, st.Message = w.stateLocked(now)
	if !w.haveFull {
		return st
	}
	if w.full.Capabilities != nil {
		st.Capabilities = proto.Clone(w.full.Capabilities).(*fleetgrpc.KubeCapabilities)
	}
	st.Warnings = append([]string(nil), w.full.Warnings...)
	for _, c := range w.full.Checks {
		st.Checks = append(st.Checks, proto.Clone(c).(*fleetgrpc.KubeCheck))
	}
	if !w.lastProbe.IsZero() {
		st.CheckedAt = timestamppb.New(w.lastProbe)
	}
	if !w.deepAt.IsZero() {
		st.DeepCheckedAt = timestamppb.New(w.deepAt)
	}
	if del := st.Capabilities.GetImageDelivery(); del != nil && im != nil {
		im.live(del, w.rec)
	}
	if st.Capabilities != nil && w.m.svc != nil && w.m.svc.kubeRecovery != nil {
		st.Capabilities.PendingTeardowns = w.m.svc.kubeRecovery.pendingTeardowns(w.rec.ID)
	}
	if w.m.svc != nil && w.m.svc.kubePods != nil {
		// A fleet moving off a node the cluster empties, or unable to.
		if moving, warnings := w.m.svc.kubePods.evac.status(w.rec.ID); moving || len(warnings) > 0 {
			if st.Capabilities == nil {
				st.Capabilities = &fleetgrpc.KubeCapabilities{}
			}
			st.Capabilities.NodeCordonedMoving = moving
			st.Warnings = append(st.Warnings, warnings...)
		}
	}
	if exp := w.cred.ExpiresAt; !exp.IsZero() {
		if st.Capabilities == nil {
			st.Capabilities = &fleetgrpc.KubeCapabilities{}
		}
		st.Capabilities.CredentialsExpireAt = timestamppb.New(exp)
		if left := exp.Sub(now); !w.cred.Refreshable && left > 0 && left <= kubeExpiryWarning {
			st.Warnings = append(st.Warnings, "credentials expire in "+preflight.Ago(left))
		}
	}
	return st
}
