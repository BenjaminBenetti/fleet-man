//go:build !nok8s

package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/mounts/syncpolicy"
	"github.com/BenjaminBenetti/fleet-man/internal/podshare"
	"github.com/BenjaminBenetti/fleet-man/internal/safefs"
	"github.com/BenjaminBenetti/fleet-man/internal/sharesync"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime/kubert"
)

// share_sync.go is the shared-state sync manager: for each fleet with
// instances on a cluster, the sharesync hub on the daemon host (its state
// in ~/.fleet/sync/<fleet>, state.SyncFleetDir), and its sides:
//
//   - "host": the fleet's folders on the daemon host (~/.fleet/workspaces/
//     <fleet>/.claude, files, .mnt, ...), the canonical copy the fleet's
//     docker instances bind;
//   - "store:<cluster>": each cluster's own logins (credential_scope is
//     location by default: logging in on a cluster serves that cluster),
//     kept on the daemon host in the login store (state.SyncLocationDir),
//     which the automation login check reads, and which a share made again
//     on that cluster (the fleet destroyed and made again) is seeded from;
//   - "k8s:<cluster>/<share>": the spoke of each share the fleet's running
//     instances mount, run in the oldest running pod of the share
//     (kubernetes.ElectShareSpoke), elected again when that pod goes; on the
//     copy tier, one per running instance.
//
// The lifecycle wiring: a create, clone, rebuild or start waits, after the
// pod agent and before the devcontainer's hooks, for the fleet's logins and
// settings to be in the pod (ready: the Ready gate, bounded, a warning when
// it runs out); a stop flushes first (the pod's last changes reach the
// host), and the share's spoke moves to another pod; a fleet destroy
// flushes, then deletes each share, or keeps it labelled an orphan when the
// flush failed; the daemon's shutdown flushes (shareFlush). Each instance's
// InstanceRuntime.share_sync carries its share's state for the rows' badge;
// `fleet sync status|wait|conflicts` read the rest (share_sync_rpc.go).
//
// Nothing here runs, nor reaches a cluster, until a fleet has an instance
// on one with fleet mounts.

// Waits and cadences (vars so tests shorten them).
var (
	// shareReadyTimeout bounds the Ready gate.
	shareReadyTimeout = 60 * time.Second
	// shareStopFlush bounds a stop's flush, shareDestroyFlush a destroy's.
	shareStopFlush    = 20 * time.Second
	shareDestroyFlush = 45 * time.Second
	// shareSyncInterval is the reconcile backstop; sharePublishInterval how
	// often the rows' state is looked at.
	shareSyncInterval    = 10 * time.Second
	sharePublishInterval = 2 * time.Second
	// shareElectTimeout bounds an election (a pod list); shareRetryMax caps
	// the backoff of a share whose spoke could not start.
	shareElectTimeout = 20 * time.Second
	shareRetryMax     = 30 * time.Second
	// shareWaitDefault bounds `fleet sync wait` without a timeout.
	shareWaitDefault = 2 * time.Minute
	// shareUsageTimeout bounds measuring a share for `fleet sync status`.
	shareUsageTimeout = 20 * time.Second
)

// The sides that are not a cluster's share.
const (
	// shareHostSide is the daemon host's side, and its location.
	shareHostSide = "host"
	// shareStorePrefix names a cluster's login store: "store:<cluster>".
	shareStorePrefix = "store:"
	// shareHostFull is the daemon host's full disk as the badge says it.
	shareHostFull = "the daemon host's disk is under 5% free"
	// shareMinFree is the share of the daemon host's disk the sync leaves.
	shareMinFree = 0.05
)

// shareSpokes is what the sync needs of the clusters (the pool; tests stand
// in their own).
type shareSpokes interface {
	// share is the share of the instance (cluster, key) as its record keeps
	// it (kubernetes.ErrNoShare without fleet mounts).
	share(cluster, key string) (kubernetes.ShareRecord, error)
	// elect is the key of the instance whose pod runs the spoke of the share
	// the instance key mounts ("" when no pod of it runs).
	elect(ctx context.Context, cluster, fleetName, key string, share kubernetes.ShareRecord) (string, error)
	// serve runs that spoke in key's pod, attached to hub as info, until it
	// ends or ctx does; attached hears once it is attached.
	serve(ctx context.Context, cluster, key string, hub sharesync.RemoteHub, info sharesync.SideInfo, attached func()) error
	// usage is the fleet's share on cluster (ok false: none there).
	usage(ctx context.Context, cluster, fleetName string) (fleet.ShareUsage, bool, error)
	// retire deletes the fleet's share on cluster, or with keep labels it an
	// orphan; it returns its name ("" when there is none).
	retire(ctx context.Context, cluster, fleetName string, keep bool) (string, error)
}

// poolSpokes is shareSpokes on the daemon's pool.
type poolSpokes struct{}

func (poolSpokes) share(cluster, key string) (kubernetes.ShareRecord, error) {
	_, s, err := kubernetes.InstanceShare(cluster, key)
	return s, err
}

func (poolSpokes) elect(ctx context.Context, cluster, fleetName, key string, share kubernetes.ShareRecord) (string, error) {
	return kubernetes.DefaultPool().ElectShareSpoke(ctx, cluster, fleetName, key, share)
}

func (poolSpokes) serve(ctx context.Context, cluster, key string, hub sharesync.RemoteHub, info sharesync.SideInfo, attached func()) error {
	return kubernetes.DefaultPool().ServeShareSpoke(ctx, cluster, key, hub, info, func(uint64) { attached() })
}

func (poolSpokes) usage(ctx context.Context, cluster, fleetName string) (fleet.ShareUsage, bool, error) {
	return kubernetes.DefaultPool().ShareUsage(ctx, cluster, fleetName)
}

func (poolSpokes) retire(ctx context.Context, cluster, fleetName string, keep bool) (string, error) {
	return kubernetes.DefaultPool().RetireShare(ctx, cluster, fleetName, keep)
}

// shareSync is the shared-state sync manager.
type shareSync struct {
	svc    *service
	spokes shareSpokes
	// records are the connections (their credential_scope and
	// share_retention); usable says whether one can be reached now (a fleet
	// destroy reaches only those).
	records func() []fleet.KubeCluster
	usable  func(ctx context.Context, id string) bool
	now     func() time.Time
	log     *slog.Logger

	wake chan struct{}

	// recMu keeps one reconcile at a time.
	recMu sync.Mutex

	mu     sync.Mutex
	fleets map[string]*fleetSync
	// urgent are the share sides a Ready gate waits for: elected at once,
	// whatever the state says of their instances.
	urgent map[string]int
	// changed is closed (and replaced) whenever a spoke attaches or ends.
	changed   chan struct{}
	published map[string]*fleetgrpc.ShareSyncStatus
	// ctx is the spokes' and the local sides': not the serve loop's, which
	// the shutdown cancels before it flushes.
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
}

func newShareSync(svc *service) *shareSync {
	ctx, cancel := context.WithCancel(context.Background())
	m := &shareSync{
		svc: svc, spokes: poolSpokes{}, now: time.Now, log: flog.L(),
		usable:    func(ctx context.Context, id string) bool { return kubert.For(id).Reachable(ctx) },
		wake:      make(chan struct{}, 1),
		fleets:    map[string]*fleetSync{},
		urgent:    map[string]int{},
		changed:   make(chan struct{}),
		published: map[string]*fleetgrpc.ShareSyncStatus{},
		ctx:       ctx, cancel: cancel,
	}
	m.records = func() []fleet.KubeCluster {
		reg, err := state.LoadKubeClusters()
		if err != nil {
			return nil
		}
		return reg.Clusters
	}
	return m
}

// fleetSync is one fleet's sync.
type fleetSync struct {
	name string
	dir  string
	// roots are the folders the hub and the host's side sync.
	roots []podshare.SpokeRoot
	hub   *sharesync.Hub
	host  *localSide
	// folders are the host's root folders as opened: one removed and made
	// again is a folder its manifest no longer describes, so the fleet's
	// sync opens again (sharesync.ErrRootMoved).
	folders map[string]os.FileInfo
	stores  map[string]*localSide
	spokes  map[string]*spokeRun
	retries map[string]*spokeRetry
	// shares and insts are the fleet's shares and cluster instances, as the
	// last reconcile found them.
	shares map[string]*shareWant
	insts  []instWant
	// syncedAt is when each share side last came into sync, inSync whether
	// it is now (as last published).
	syncedAt map[string]time.Time
	inSync   map[string]bool
	// unflushed are the share sides a stop could not flush, or a destroy
	// kept as orphans (persisted, past the destroy): a destroy keeps such a
	// share, until a flush takes what it holds.
	unflushed map[string]bool
	// conflicts counts the conflict copies, as of conflictsAt.
	conflicts   int
	conflictsAt time.Time
}

// localSide is a side on the daemon host: its tree and its scan loop.
type localSide struct {
	info sharesync.SideInfo
	tree *sharesync.Tree
	side *sharesync.Side
	stop func()
}

// spokeRun is a share's spoke being served.
type spokeRun struct {
	key      string
	info     sharesync.SideInfo
	cancel   context.CancelFunc
	done     chan struct{}
	attached atomic.Bool
	err      atomic.Pointer[error]
}

// ended reports whether the spoke's connection is gone.
func (r *spokeRun) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// spokeRetry is a share whose spoke could not start: when to try again.
type spokeRetry struct {
	at  time.Time
	n   int
	err string
	// noPod: the election found no pod of the share running.
	noPod bool
}

// errNoSpokePod is an election that found no pod of the share running.
var errNoSpokePod = errors.New("no pod of the share runs")

// shareWant is one share of a fleet as the state has it.
type shareWant struct {
	cluster string
	share   kubernetes.ShareRecord
	info    sharesync.SideInfo
	// keys are the instances that mount it, in name order.
	keys []string
	// active: one of them runs, or is coming up.
	active bool
}

// instWant is one instance of a fleet on a cluster.
type instWant struct {
	name, cluster, key, side string
	running                  bool
}

// fleetWant is what a fleet's sync is to be.
type fleetWant struct {
	roots    []podshare.SpokeRoot
	clusters map[string]bool
	shares   map[string]*shareWant
	insts    []instWant
}

// signal asks the loop to reconcile.
func (m *shareSync) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// onState hands in a new state (the hub's state listener; it must not
// block).
func (m *shareSync) onState(*state.State) { m.signal() }

// notify tells those waiting on a spoke that one attached or ended.
func (m *shareSync) notify() {
	m.mu.Lock()
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
	m.signal()
}

// run reconciles on state changes and the backstop, and keeps the rows'
// state current, until ctx ends. What it opened stays open for the
// shutdown's flush (flushAndClose).
func (m *shareSync) run(ctx context.Context) {
	backstop := time.NewTicker(shareSyncInterval)
	defer backstop.Stop()
	publish := time.NewTicker(sharePublishInterval)
	defer publish.Stop()
	m.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
			m.reconcile(ctx)
		case <-backstop.C:
			m.reconcile(ctx)
		case <-publish.C:
		}
		m.publish()
	}
}

// shareActive reports whether an instance of status s runs, is coming up,
// or still runs on its way out (a stop or a destroy flushes it first): its
// share's spoke is to be served.
func shareActive(s fleet.InstanceStatus) bool {
	switch s {
	case fleet.StatusRunning, fleet.StatusCreating, fleet.StatusCloning, fleet.StatusRebuilding, fleet.StatusStarting,
		fleet.StatusStopping, fleet.StatusDeleting:
		return true
	}
	return false
}

// wants is what each fleet's sync is to be, from st: a fleet with an
// instance on a cluster that has fleet mounts there has one.
func (m *shareSync) wants(st *state.State) map[string]*fleetWant {
	out := map[string]*fleetWant{}
	m.mu.Lock()
	urgent := make(map[string]bool, len(m.urgent))
	for id := range m.urgent {
		urgent[id] = true
	}
	m.mu.Unlock()
	logins := m.loginsByCluster()
	for name, f := range st.Fleets {
		if f == nil {
			continue
		}
		insts := slices.Clone(f.Instances)
		sort.Slice(insts, func(i, j int) bool { return insts[i].Name < insts[j].Name })
		for _, inst := range insts {
			if inst == nil || !isKubeInstance(inst) {
				continue
			}
			share, err := m.spokes.share(inst.KubeCluster, inst.KubeKey)
			if err != nil {
				if !errors.Is(err, kubernetes.ErrNoShare) && !errors.Is(err, fs.ErrNotExist) {
					m.log.Debug("share sync: reading an instance's share", "fleet", name, "instance", inst.Name, "err", err)
				}
				continue
			}
			if len(share.Roots) == 0 {
				continue
			}
			w := out[name]
			if w == nil {
				w = &fleetWant{clusters: map[string]bool{}, shares: map[string]*shareWant{}}
				out[name] = w
			}
			w.roots = mergeRoots(w.roots, share.Roots)
			w.clusters[inst.KubeCluster] = true
			info := spokeInfo(inst.KubeCluster, inst.KubeKey, share, logins[inst.KubeCluster])
			sw := w.shares[info.ID]
			if sw == nil {
				sw = &shareWant{cluster: inst.KubeCluster, share: share, info: info}
				w.shares[info.ID] = sw
			}
			sw.keys = append(sw.keys, inst.KubeKey)
			sw.active = sw.active || shareActive(inst.Status) || urgent[info.ID]
			w.insts = append(w.insts, instWant{name: inst.Name, cluster: inst.KubeCluster, key: inst.KubeKey, side: info.ID, running: inst.Status == fleet.StatusRunning})
		}
	}
	return out
}

// spokeInfo is the side a share's spoke is attached as: its location the
// connection, and logins the location whose logins it keeps ("" its own).
func spokeInfo(cluster, key string, share kubernetes.ShareRecord, logins string) sharesync.SideInfo {
	info := kubernetes.ShareSide(cluster, key, share)
	info.Logins = logins
	return info
}

// loginsByCluster is the location whose logins each connection's sides keep:
// the daemon host's for one that shares them (credential_scope fleet,
// experimental), else its own ("", missing from the map).
func (m *shareSync) loginsByCluster() map[string]string {
	out := map[string]string{}
	for _, rec := range m.records() {
		if rec.CredentialScope == fleet.KubeCredentialsPerFleet {
			out[rec.ID] = shareHostSide
		}
	}
	return out
}

// recordOf is connection id's record.
func (m *shareSync) recordOf(id string) (fleet.KubeCluster, bool) {
	for _, rec := range m.records() {
		if rec.ID == id {
			return rec, true
		}
	}
	return fleet.KubeCluster{}, false
}

// mergeRoots adds roots to have, by name: their reserved and skipped paths
// joined. The result is in name order.
func mergeRoots(have, roots []podshare.SpokeRoot) []podshare.SpokeRoot {
	out := slices.Clone(have)
	for _, r := range roots {
		i := slices.IndexFunc(out, func(o podshare.SpokeRoot) bool { return o.Name == r.Name })
		if i < 0 {
			out = append(out, podshare.SpokeRoot{Name: r.Name, Reserved: slices.Clone(r.Reserved), Skip: slices.Clone(r.Skip)})
			continue
		}
		for _, p := range r.Reserved {
			if !slices.Contains(out[i].Reserved, p) {
				out[i].Reserved = append(out[i].Reserved, p)
			}
		}
		for _, p := range r.Skip {
			if !slices.Contains(out[i].Skip, p) {
				out[i].Skip = append(out[i].Skip, p)
			}
		}
	}
	for i := range out {
		slices.Sort(out[i].Reserved)
		slices.Sort(out[i].Skip)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sameRoots(a, b []podshare.SpokeRoot) bool {
	return slices.EqualFunc(a, b, func(x, y podshare.SpokeRoot) bool {
		return x.Name == y.Name && slices.Equal(x.Reserved, y.Reserved) && slices.Equal(x.Skip, y.Skip)
	})
}

// reconcile converges every fleet's sync on the state.
func (m *shareSync) reconcile(ctx context.Context) {
	m.recMu.Lock()
	defer m.recMu.Unlock()
	if m.isClosed() {
		return
	}
	st, err := state.Load()
	if err != nil {
		return
	}
	wants := m.wants(st)
	m.mu.Lock()
	var gone []*fleetSync
	for name, fs := range m.fleets {
		if wants[name] == nil {
			gone = append(gone, fs)
			delete(m.fleets, name)
		}
	}
	m.mu.Unlock()
	for _, fs := range gone {
		m.closeFleet(fs)
	}
	names := make([]string, 0, len(wants))
	for name := range wants {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m.converge(ctx, name, wants[name])
	}
}

func (m *shareSync) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// converge brings one fleet's sync to w.
func (m *shareSync) converge(ctx context.Context, name string, w *fleetWant) {
	m.mu.Lock()
	fs := m.fleets[name]
	m.mu.Unlock()
	if fs != nil && (!sameRoots(fs.roots, w.roots) || fs.moved()) {
		// New folders (a mount the fleet's settings added, taken up by a
		// rebuild), or a host folder made again: open it all again; the
		// hub's state and the manifests carry over.
		m.mu.Lock()
		delete(m.fleets, name)
		m.mu.Unlock()
		m.closeFleet(fs)
		fs = nil
	}
	if fs == nil {
		var err error
		if fs, err = m.openFleet(name, w.roots); err != nil {
			m.log.Warn("share sync: opening the fleet's sync", "fleet", name, "err", err)
			return
		}
		m.mu.Lock()
		m.fleets[name] = fs
		m.mu.Unlock()
		flog.Info("share sync: syncing the fleet's shared folders with its clusters", "fleet", name, "roots", rootNames(w.roots))
	}
	logins := m.loginsByCluster()
	m.convergeStores(fs, w, logins)
	m.convergeSpokes(ctx, fs, w)
	fs.shares, fs.insts = w.shares, w.insts
}

func rootNames(roots []podshare.SpokeRoot) []string {
	out := make([]string, len(roots))
	for i, r := range roots {
		out[i] = r.Name
	}
	return out
}

// openFleet opens a fleet's hub and the daemon host's side.
func (m *shareSync) openFleet(name string, roots []podshare.SpokeRoot) (*fleetSync, error) {
	if err := fleet.ValidateFleetName(name); err != nil {
		return nil, err
	}
	dir := state.SyncFleetDir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	hub, err := sharesync.OpenHub(dir, syncpolicy.Defaults(), rootNames(roots), sharesync.HubOptions{Logger: m.log.With("fleet", name), MinFree: shareMinFree})
	if err != nil {
		return nil, err
	}
	fs := &fleetSync{
		name: name, dir: dir, roots: roots, hub: hub,
		folders: map[string]os.FileInfo{}, stores: map[string]*localSide{}, spokes: map[string]*spokeRun{},
		retries: map[string]*spokeRetry{}, syncedAt: map[string]time.Time{}, inSync: map[string]bool{}, unflushed: loadUnflushed(dir),
	}
	host, folders, err := m.openHost(name, dir, roots, hub)
	if err != nil {
		hub.Close()
		return nil, err
	}
	fs.host, fs.folders = host, folders
	return fs, nil
}

// openHost opens the daemon host's side of a fleet: its folders under
// WorkspacesDir (made when missing, never through a symlink), the JSON
// settings files the resolver left empty seeded '{}' first (an empty one is
// a write under way to the sync, which would never take the share's copy
// over it), attached to hub and running.
func (m *shareSync) openHost(name, dir string, roots []podshare.SpokeRoot, hub *sharesync.Hub) (*localSide, map[string]os.FileInfo, error) {
	base := filepath.Join(state.WorkspacesDir(), name)
	if err := os.MkdirAll(base, 0o777); err != nil {
		return nil, nil, err
	}
	top, err := safefs.Open(base, safefs.Options{})
	if err != nil {
		return nil, nil, err
	}
	defer top.Close()
	table := syncpolicy.Defaults()
	folders := map[string]os.FileInfo{}
	var trees []sharesync.TreeRoot
	for _, r := range roots {
		if err := top.MkdirAll(r.Name, 0o777); err != nil {
			return nil, nil, fmt.Errorf("the fleet's folder %s: %w", r.Name, err)
		}
		fi, err := top.Lstat(r.Name)
		if err != nil {
			return nil, nil, err
		}
		folders[r.Name] = fi
		seedJSON(top, table, r)
		trees = append(trees, sharesync.TreeRoot{Name: r.Name, Dir: filepath.Join(base, filepath.FromSlash(r.Name)), Reserved: r.Reserved, Skip: r.Skip})
	}
	side, err := m.openLocal(hub, sharesync.SideInfo{ID: shareHostSide, Location: shareHostSide}, trees, filepath.Join(dir, "host"), nil)
	if err != nil {
		return nil, nil, err
	}
	return side, folders, nil
}

// seedJSON writes '{}' into each of root's reserved JSON settings files
// (json3) that is empty on the daemon host.
func seedJSON(top *safefs.Root, table syncpolicy.Table, r podshare.SpokeRoot) {
	for _, p := range r.Reserved {
		if policy, ok := table.PolicyFor(path.Join(r.Name, p)); !ok || policy != syncpolicy.JSON3 {
			continue
		}
		name := path.Join(r.Name, p)
		fi, err := top.Lstat(name)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != 0 {
			continue
		}
		if _, err := top.WriteFile(name, strings.NewReader("{}"), 0o666); err != nil {
			flog.Warn("share sync: seeding an empty settings file", "file", name, "err", err)
			continue
		}
		_ = top.Chmod(name, 0o666)
	}
}

// moved reports whether a host folder of the fleet's is no longer the one
// its side opened.
func (fs *fleetSync) moved() bool {
	base := filepath.Join(state.WorkspacesDir(), fs.name)
	for name, was := range fs.folders {
		now, err := os.Lstat(filepath.Join(base, filepath.FromSlash(name)))
		if err != nil || !os.SameFile(was, now) {
			return true
		}
	}
	return false
}

// openLocal opens a side on the daemon host over trees, its manifests in
// stateDir, attaches it to hub and runs it; changed hears of every change
// applied to it (nil: none).
func (m *shareSync) openLocal(hub *sharesync.Hub, info sharesync.SideInfo, trees []sharesync.TreeRoot, stateDir string, changed func()) (*localSide, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	tree, err := sharesync.OpenTree(trees, sharesync.TreeOptions{
		StateDir: stateDir, MinFree: shareMinFree, FullReason: shareHostFull, Logger: m.log.With("side", info.ID),
	})
	if err != nil {
		return nil, err
	}
	side := sharesync.NewSide(info, tree, hub, sharesync.SideOptions{Logger: m.log.With("side", info.ID)})
	var peer sharesync.Peer = side
	if changed != nil {
		peer = notifyPeer{Side: side, changed: changed}
	}
	w, err := hub.Attach(m.ctx, info, peer, tree.Hello())
	if err == nil {
		err = tree.Welcome(w)
	}
	if err != nil {
		hub.Detach(info.ID)
		tree.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(m.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = side.Run(ctx)
	}()
	return &localSide{info: info, tree: tree, side: side, stop: func() { cancel(); <-done }}, nil
}

// notifyPeer is a side that tells of what was applied to it.
type notifyPeer struct {
	*sharesync.Side
	changed func()
}

func (p notifyPeer) Apply(ctx context.Context, b sharesync.Batch) ([]sharesync.Result, error) {
	results, err := p.Side.Apply(ctx, b)
	for _, r := range results {
		if r.Status == sharesync.Applied {
			p.changed()
			break
		}
	}
	return results, err
}

// convergeStores keeps a login store per cluster the fleet is on.
func (m *shareSync) convergeStores(fs *fleetSync, w *fleetWant, logins map[string]string) {
	for cluster, store := range fs.stores {
		if !w.clusters[cluster] || store.info.Logins != logins[cluster] {
			store.stop()
			fs.hub.Detach(store.info.ID)
			store.tree.Close()
			delete(fs.stores, cluster)
		}
	}
	for cluster := range w.clusters {
		if fs.stores[cluster] != nil {
			continue
		}
		store, err := m.openStore(fs, cluster, logins[cluster])
		if err != nil {
			m.log.Warn("share sync: opening a cluster's login store", "fleet", fs.name, "cluster", cluster, "err", err)
			continue
		}
		if store != nil {
			fs.stores[cluster] = store
		}
	}
}

// openStore opens cluster's login store of the fleet: a side over the
// store's folders that keeps only that location's logins (nil when none of
// the fleet's folders holds a login: only custom mounts, say).
func (m *shareSync) openStore(fs *fleetSync, cluster, logins string) (*localSide, error) {
	table := syncpolicy.Defaults()
	dir := state.SyncLocationDir(fs.name, cluster)
	var trees []sharesync.TreeRoot
	for _, r := range fs.roots {
		root, inner, ok := table.Root(r.Name)
		if !ok || inner != "" && inner != "." || !hasCredential(root) {
			continue
		}
		folder := filepath.Join(dir, filepath.FromSlash(r.Name))
		if err := os.MkdirAll(folder, 0o700); err != nil {
			return nil, err
		}
		trees = append(trees, sharesync.TreeRoot{Name: r.Name, Dir: folder})
	}
	if len(trees) == 0 {
		return nil, nil
	}
	info := sharesync.SideInfo{ID: shareStorePrefix + cluster, Location: cluster, Logins: logins, LoginsOnly: true}
	return m.openLocal(fs.hub, info, trees, filepath.Join(fs.dir, "state", "loc", stateSegment(cluster)), m.loginsChanged)
}

// stateSegment is name as one path segment of the sync's state folders.
func stateSegment(name string) string {
	return filepath.Base(state.SyncLocationDir("x", name))
}

func hasCredential(root syncpolicy.Root) bool {
	return slices.ContainsFunc(root.Rules, func(r syncpolicy.Rule) bool { return r.Policy == syncpolicy.Credential })
}

// loginsChanged is a login store taking a login or a logout: the automation
// login check reads the stores, so the scheduler looks again (its status,
// the refusals it holds).
func (m *shareSync) loginsChanged() {
	if m.svc != nil {
		m.svc.wakeScheduler()
	}
}

// convergeSpokes serves the spoke of each active share of the fleet.
func (m *shareSync) convergeSpokes(ctx context.Context, fs *fleetSync, w *fleetWant) {
	for id, run := range fs.spokes {
		sw := w.shares[id]
		switch {
		case run.ended():
			m.ended(fs, id, run)
		case sw == nil || !sw.active || sw.info != run.info || !slices.Contains(sw.keys, run.key):
			// Its share is gone or idle, its instance no longer mounts it,
			// or it is to be attached as another side (the connection's
			// credential_scope changed).
			run.cancel()
		}
	}
	ids := make([]string, 0, len(w.shares))
	for id := range w.shares {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		sw := w.shares[id]
		if !sw.active || fs.spokes[id] != nil {
			continue
		}
		m.mu.Lock()
		urgent := m.urgent[id] > 0
		m.mu.Unlock()
		if r := fs.retries[id]; r != nil && !urgent && m.now().Before(r.at) {
			continue
		}
		ectx, cancel := context.WithTimeout(ctx, shareElectTimeout)
		key, err := m.spokes.elect(ectx, sw.cluster, fs.name, sw.keys[0], sw.share)
		cancel()
		if err == nil && key == "" {
			err = errNoSpokePod
		}
		if err != nil {
			m.retry(fs, id, err)
			continue
		}
		m.startSpoke(fs, sw, key)
	}
}

// startSpoke serves sw's spoke in key's pod.
func (m *shareSync) startSpoke(fs *fleetSync, sw *shareWant, key string) {
	ctx, cancel := context.WithCancel(m.ctx)
	run := &spokeRun{key: key, info: sw.info, cancel: cancel, done: make(chan struct{})}
	fs.spokes[sw.info.ID] = run
	hub, cluster := fs.hub, sw.cluster
	go func() {
		defer close(run.done)
		err := m.spokes.serve(ctx, cluster, key, hub, run.info, func() {
			run.attached.Store(true)
			m.notify()
		})
		if err == nil {
			err = errors.New("the spoke's connection ended")
		}
		run.err.Store(&err)
		// Gone until a spoke attaches as it again: the hub holds nothing
		// back for it meanwhile, and keeps what it held.
		hub.Detach(run.info.ID)
		m.notify()
	}()
}

// ended drops a spoke run that ended, and backs its share off when it never
// attached.
func (m *shareSync) ended(fs *fleetSync, id string, run *spokeRun) {
	delete(fs.spokes, id)
	if run.attached.Load() {
		delete(fs.retries, id)
		return
	}
	err := errors.New("the spoke ended")
	if p := run.err.Load(); p != nil {
		err = *p
	}
	m.retry(fs, id, err)
}

// retry backs a share off after its spoke could not start.
func (m *shareSync) retry(fs *fleetSync, id string, err error) {
	r := fs.retries[id]
	if r == nil {
		r = &spokeRetry{}
		fs.retries[id] = r
	}
	r.n++
	d := min(time.Second<<min(r.n-1, 10), shareRetryMax)
	r.at = m.now().Add(d)
	r.noPod = errors.Is(err, errNoSpokePod)
	if r.err != err.Error() {
		m.log.Info("share sync: a share's spoke is not up", "fleet", fs.name, "side", id, "err", err, "retry_in", d)
	}
	r.err = err.Error()
}

// closeFleet stops a fleet's sync: its spokes, its sides, its hub, then the
// trees.
func (m *shareSync) closeFleet(fs *fleetSync) {
	for _, run := range fs.spokes {
		run.cancel()
	}
	for _, run := range fs.spokes {
		select {
		case <-run.done:
		case <-time.After(5 * time.Second):
			m.log.Warn("share sync: a spoke outlived its fleet's sync", "fleet", fs.name, "side", run.info.ID)
		}
	}
	sides := []*localSide{fs.host}
	for _, store := range fs.stores {
		sides = append(sides, store)
	}
	for _, s := range sides {
		if s != nil {
			s.stop()
		}
	}
	if err := fs.hub.Close(); err != nil {
		m.log.Warn("share sync: closing the fleet's hub", "fleet", fs.name, "err", err)
	}
	for _, s := range sides {
		if s != nil {
			s.tree.Close()
		}
	}
}

// fleetOf is a fleet's open sync, or nil.
func (m *shareSync) fleetOf(name string) *fleetSync {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fleets[name]
}

// spokeAttached reports whether the spoke of share side id of fleetName is
// attached, with why not.
func (m *shareSync) spokeAttached(fleetName, id string) (bool, string) {
	m.recMu.Lock()
	defer m.recMu.Unlock()
	fs := m.fleetOf(fleetName)
	if fs == nil {
		return false, "the fleet's sync is not open"
	}
	if run := fs.spokes[id]; run != nil {
		if run.attached.Load() && !run.ended() {
			return true, ""
		}
		if p := run.err.Load(); p != nil {
			return false, (*p).Error()
		}
		return false, "the share's spoke is starting"
	}
	if r := fs.retries[id]; r != nil {
		return false, r.err
	}
	return false, "the share's spoke is not elected yet"
}

// changedCh is closed at the next spoke attach or end.
func (m *shareSync) changedCh() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

// hotPath reports a path the Ready gate waits for: the logins, settings
// and ~/.claude.json (the files a devcontainer's hooks and the agents read
// first), not the transcripts and custom mounts that follow.
func hotPath(table syncpolicy.Table) func(root, p string) bool {
	return func(root, p string) bool {
		switch policy, _ := table.PolicyFor(path.Join(root, p)); policy {
		case syncpolicy.Credential, syncpolicy.JSON3, syncpolicy.TOML3:
			return true
		}
		return false
	}
}

// ready is the Ready gate (backend.Hooks.SharedStateReady and the start's
// kubernetes.Environment.ShareReady): it waits, within shareReadyTimeout,
// for the share of fleetName/instance to have its spoke attached and to hold
// the fleet's logins and settings. An instance without fleet mounts on a
// cluster passes at once.
func (m *shareSync) ready(ctx context.Context, fleetName, instance string) error {
	_, _, inst, err := loadShareInstance(fleetName, instance)
	if err != nil || !isKubeInstance(inst) {
		return nil
	}
	share, err := m.spokes.share(inst.KubeCluster, inst.KubeKey)
	if err != nil || len(share.Roots) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, shareReadyTimeout)
	defer cancel()
	id := spokeInfo(inst.KubeCluster, inst.KubeKey, share, "").ID
	m.mu.Lock()
	m.urgent[id]++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.urgent[id]--; m.urgent[id] <= 0 {
			delete(m.urgent, id)
		}
		m.mu.Unlock()
	}()
	start := m.now()
	for {
		changed := m.changedCh()
		m.reconcile(ctx)
		ok, why := m.spokeAttached(fleetName, id)
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the fleet's share did not connect within %s: %s", shareReadyTimeout, why)
		case <-changed:
		case <-time.After(500 * time.Millisecond):
		}
	}
	fs := m.fleetOf(fleetName)
	if fs == nil {
		return errors.New("the fleet's sync closed")
	}
	if err := fs.hub.FlushSide(ctx, id, hotPath(syncpolicy.Defaults())); err != nil {
		return fmt.Errorf("the fleet's logins and settings are not all in the instance yet: %w", err)
	}
	flog.Info("share sync: the instance has the fleet's shared state", "fleet", fleetName, "instance", instance, "side", id, "ms", flog.MillisSince(start))
	return nil
}

// loadShareInstance loads the state and fleetName/instance's record.
func loadShareInstance(fleetName, instance string) (*state.State, *fleet.Fleet, *fleet.Instance, error) {
	st, err := state.Load()
	if err != nil {
		return nil, nil, nil, err
	}
	f, ok := st.Fleets[fleetName]
	if !ok || f == nil {
		return nil, nil, nil, fmt.Errorf("fleet %q not found", fleetName)
	}
	inst, err := f.GetInstance(instance)
	if err != nil {
		return nil, nil, nil, err
	}
	return st, f, inst, nil
}

// waitAttached waits, within ctx, for the spokes of the fleet's active
// shares to be attached; it returns the shares whose spoke is not, with why.
// A share none of whose pods runs (a failed create's, a stopped one's) is
// not waited for: nothing there can change; nor is one on a cluster that
// cannot be reached now (it is named at once).
func (m *shareSync) waitAttached(ctx context.Context, fleetName string) []string {
	m.recMu.Lock()
	if fs := m.fleetOf(fleetName); fs != nil {
		// Elect now what is backed off: the answer is wanted fresh.
		for _, r := range fs.retries {
			r.at = time.Time{}
		}
	}
	m.recMu.Unlock()
	for {
		changed := m.changedCh()
		m.reconcile(ctx)
		var missing []string
		waiting := false
		m.recMu.Lock()
		if fs := m.fleetOf(fleetName); fs != nil {
			for id, sw := range fs.shares {
				if !sw.active {
					continue
				}
				run := fs.spokes[id]
				if run != nil && run.attached.Load() && !run.ended() {
					continue
				}
				why := "not connected"
				if r := fs.retries[id]; run == nil && r != nil {
					if r.noPod {
						continue
					}
					why = r.err
				}
				if !m.usable(ctx, sw.cluster) {
					why = "its cluster cannot be reached now"
				} else {
					waiting = true
				}
				missing = append(missing, id+": "+why)
			}
		}
		m.recMu.Unlock()
		sort.Strings(missing)
		if !waiting {
			return missing
		}
		select {
		case <-ctx.Done():
			return missing
		case <-changed:
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// flushFleet waits, within ctx, for every side of fleetName's sync to agree
// (spokes of its active shares attached first). A fleet without a sync is
// in sync.
func (m *shareSync) flushFleet(ctx context.Context, fleetName string) error {
	missing := m.waitAttached(ctx, fleetName)
	fs := m.fleetOf(fleetName)
	if fs == nil {
		return nil
	}
	attached := m.attachedUnflushed(fs)
	err := fs.hub.Flush(ctx)
	if err == nil {
		m.forgetUnflushed(fs, attached)
	}
	if len(missing) == 0 {
		return err
	}
	fe := &sharesync.FlushError{}
	if !errors.As(err, &fe) {
		fe = &sharesync.FlushError{}
	}
	fe.Pending = append(missing, fe.Pending...)
	return fe
}

// beforeStop flushes the fleet of an instance that is about to stop, so the
// pod's last changes are on the daemon host (bounded; a failure is logged,
// and the stop goes on). Its share's spoke moves to another of its pods once
// this one is gone.
func (m *shareSync) beforeStop(fleetName, instance string) {
	m.flushBefore(fleetName, instance, "stop", shareStopFlush)
}

// flushBefore flushes the fleet of an instance about to go (verb) within d;
// a share that could not be flushed is remembered (a destroy keeps it).
func (m *shareSync) flushBefore(fleetName, instance, verb string, d time.Duration) {
	_, _, inst, err := loadShareInstance(fleetName, instance)
	if err != nil || !isKubeInstance(inst) {
		return
	}
	share, err := m.spokes.share(inst.KubeCluster, inst.KubeKey)
	if err != nil || len(share.Roots) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	id := spokeInfo(inst.KubeCluster, inst.KubeKey, share, "").ID
	err = m.flushFleet(ctx, fleetName)
	// Only what holds this cluster's share back counts against it: another
	// cluster's share being behind (or out of reach) leaves this one's
	// changes on the daemon host all the same.
	behind := flushOutcome(err, nil).keeps(inst.KubeCluster)
	m.markUnflushed(fleetName, id, behind)
	switch {
	case behind:
		flog.Warn("share sync: the instance's last changes may not be on the daemon host", "fleet", fleetName, "instance", instance, "before", verb, "err", err)
	case err != nil:
		flog.Info("share sync: the fleet's other shares are not all in sync", "fleet", fleetName, "instance", instance, "before", verb, "err", err)
	}
}

// attachedUnflushed are the spokes, attached now, of the shares of fs a
// stop could not flush.
func (m *shareSync) attachedUnflushed(fs *fleetSync) map[string]*spokeRun {
	m.recMu.Lock()
	defer m.recMu.Unlock()
	out := map[string]*spokeRun{}
	for id := range fs.unflushed {
		if run := fs.spokes[id]; run != nil && run.attached.Load() && !run.ended() {
			out[id] = run
		}
	}
	return out
}

// forgetUnflushed clears the unflushed mark of each share whose spoke was
// attached (attached, attachedUnflushed's) all through a flush that just
// succeeded: the daemon host now has everything that share held.
func (m *shareSync) forgetUnflushed(fs *fleetSync, attached map[string]*spokeRun) {
	if len(attached) == 0 {
		return
	}
	m.recMu.Lock()
	defer m.recMu.Unlock()
	if m.fleetOf(fs.name) != fs {
		// Opened again meanwhile: what this one saw is no longer current.
		return
	}
	changed := false
	for id, run := range attached {
		if fs.spokes[id] == run && !run.ended() && fs.unflushed[id] {
			delete(fs.unflushed, id)
			changed = true
		}
	}
	if changed {
		saveUnflushed(fs.dir, fs.unflushed)
	}
}

// markUnflushed records (or clears) that share side id of fleetName holds
// changes the daemon host may not have.
func (m *shareSync) markUnflushed(fleetName, id string, unflushed bool) {
	m.recMu.Lock()
	defer m.recMu.Unlock()
	fs := m.fleetOf(fleetName)
	if fs == nil || fs.unflushed[id] == unflushed {
		return
	}
	if unflushed {
		fs.unflushed[id] = true
	} else {
		delete(fs.unflushed, id)
	}
	saveUnflushed(fs.dir, fs.unflushed)
}

// unflushedFile is where a fleet's unflushed shares are kept.
func unflushedFile(dir string) string { return filepath.Join(dir, "unflushed") }

func loadUnflushed(dir string) map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile(unflushedFile(dir))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out[line] = true
		}
	}
	return out
}

func saveUnflushed(dir string, set map[string]bool) {
	if len(set) == 0 {
		if err := os.Remove(unflushedFile(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			flog.Warn("share sync: recording the unflushed shares", "err", err)
		}
		return
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if err := atomicfile.Write(unflushedFile(dir), []byte(strings.Join(ids, "\n")+"\n"), 0o600); err != nil {
		flog.Warn("share sync: recording the unflushed shares", "err", err)
	}
}

// fleetFlush is a fleet destroy's flush, and what it found.
type fleetFlush struct {
	// flushed: the daemon host holds everything the fleet's shares did;
	// else behind are the clusters whose share it may not have all of
	// (every cluster when all is set: the host or a login store is behind).
	flushed bool
	all     bool
	behind  map[string]bool
	err     error
}

// keeps reports whether the destroy keeps cluster's share.
func (ff fleetFlush) keeps(cluster string) bool {
	return !ff.flushed && (ff.all || ff.behind[cluster])
}

// flushOutcome is what a flush that ended with err (and the shares a stop
// could not flush) says of each cluster's share: a line naming a share's
// side ("k8s:<cluster>/...") puts that cluster behind, any other (the host,
// a login store, a root over its cap) every one.
func flushOutcome(err error, unflushed map[string]bool) fleetFlush {
	ff := fleetFlush{behind: map[string]bool{}, err: err}
	var lines []string
	if err != nil {
		var fe *sharesync.FlushError
		if errors.As(err, &fe) {
			lines = fe.Pending
		} else {
			ff.all = true
		}
	}
	for id := range unflushed {
		lines = append(lines, id+": may hold changes this host does not have")
	}
	for _, line := range lines {
		cluster, ok := sideCluster(line)
		if !ok {
			ff.all = true
			continue
		}
		ff.behind[cluster] = true
	}
	if len(unflushed) > 0 && ff.err == nil {
		ff.err = fmt.Errorf("%s may hold changes this host does not have", strings.Join(sortedKeys(unflushed), ", "))
	}
	ff.flushed = !ff.all && len(ff.behind) == 0
	return ff
}

// beforeDestroy flushes what a destroy is about to tear down: for a fleet
// destroy every side (destroyFleet), for one instance its fleet's. A failed
// fleet flush keeps the shares (retireFleet).
func (m *shareSync) beforeDestroy(fleetName, instance string, destroyFleet bool) fleetFlush {
	if !destroyFleet {
		m.flushBefore(fleetName, instance, "destroy", shareStopFlush)
		return fleetFlush{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), shareDestroyFlush)
	defer cancel()
	// No instance there syncs now (all stopped): what their stops flushed
	// is all there is, unless one could not.
	err := m.flushFleet(ctx, fleetName)
	unflushed := loadUnflushed(state.SyncFleetDir(fleetName))
	m.recMu.Lock()
	if fs := m.fleetOf(fleetName); fs != nil {
		unflushed = maps.Clone(fs.unflushed)
	}
	m.recMu.Unlock()
	return flushOutcome(err, unflushed)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// retireFleet is a fleet destroy's end, once its instances are down: on
// every connection that can be reached, its share is deleted when ff
// flushed it (share_retention delete, the default), else kept labelled an
// orphan, and stays marked unflushed; a connection that retains shares
// keeps it as it is. The fleet's sync closes (its state on the daemon host
// stays, logins included).
func (m *shareSync) retireFleet(fleetName string, ff fleetFlush) []string {
	m.recMu.Lock()
	m.mu.Lock()
	fs := m.fleets[fleetName]
	delete(m.fleets, fleetName)
	m.mu.Unlock()
	if fs != nil {
		m.closeFleet(fs)
	}
	m.recMu.Unlock()
	dir := state.SyncFleetDir(fleetName)
	// What stays marked unflushed past the destroy: the shares it kept, and
	// what was marked on the connections it did not retire (out of reach, or
	// retaining shares), so a fleet made again under the name keeps them
	// too until a flush has them (forgetUnflushed).
	unflushed, next, retired := loadUnflushed(dir), map[string]bool{}, map[string]bool{}
	var warnings []string
	for _, rec := range m.records() {
		id := rec.ID
		if rec.ShareRetention == fleet.KubeShareRetain {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), shareUsageTimeout)
		if !m.usable(ctx, id) {
			cancel()
			continue
		}
		keep := ff.keeps(id)
		claim, err := m.spokes.retire(ctx, id, fleetName, keep)
		cancel()
		switch {
		case err != nil:
			warnings = append(warnings, fmt.Sprintf("the fleet's share on %s: %v", clusterLabel(rec), err))
		case claim != "" && keep:
			next[shareSideOf(id, claim)] = true
			retired[id] = true
			warnings = append(warnings, fmt.Sprintf("kept the fleet's share %s on %s (labelled an orphan; `fleet cluster gc --apply` deletes it): its last changes did not reach this host: %v", claim, clusterLabel(rec), ff.err))
		default:
			retired[id] = true
		}
	}
	for side := range unflushed {
		if cluster, ok := sideCluster(side); !ok || !retired[cluster] {
			next[side] = true
		}
	}
	saveUnflushed(dir, next)
	return warnings
}

// shareSideOf is the side of a fleet's share (claim) on cluster, as
// kubernetes.ShareSide names it for its instances.
func shareSideOf(cluster, claim string) string { return "k8s:" + cluster + "/" + claim }

// sideCluster is the cluster of a share's side ("k8s:<cluster>/..."); ok is
// false for the host's side and the login stores.
func sideCluster(side string) (string, bool) {
	rest, ok := strings.CutPrefix(side, "k8s:")
	if !ok {
		return "", false
	}
	cluster, _, found := strings.Cut(rest, "/")
	return cluster, found
}

func clusterLabel(rec fleet.KubeCluster) string {
	if rec.DisplayName != "" {
		return rec.DisplayName
	}
	return rec.ID
}

// kickCluster retries the fleet's sides on cluster at once (its share grew).
func (m *shareSync) kickCluster(cluster, fleetName string) {
	fs := m.fleetOf(fleetName)
	if fs == nil {
		return
	}
	st := fs.hub.Status()
	for _, r := range st.Roots {
		for _, side := range r.Sides {
			if side.Location == cluster && side.Paused != "" {
				fs.hub.Kick(side.Side)
			}
		}
	}
}

// flushAndClose is the shutdown's step: every fleet's sync flushed within
// ctx, then closed. It must not depend on the serve loop's context.
func (m *shareSync) flushAndClose(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	// A reconcile under way ends soon: the jobs' and the loop's contexts
	// are gone.
	m.recMu.Lock()
	defer m.recMu.Unlock()
	m.mu.Lock()
	fleets := make([]*fleetSync, 0, len(m.fleets))
	for _, fs := range m.fleets {
		fleets = append(fleets, fs)
	}
	m.fleets = map[string]*fleetSync{}
	m.mu.Unlock()
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for _, fs := range fleets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fs.hub.Flush(ctx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", fs.name, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, fs := range fleets {
		m.closeFleet(fs)
	}
	m.cancel()
	return errors.Join(errs...)
}

// --- the rows' state ---

// publish keeps each cluster instance's InstanceRuntime.share_sync current
// while a TUI watches the runtime (what it was last told stays as it is
// meanwhile, and what changed goes at the next look).
func (m *shareSync) publish() {
	if m.svc == nil || m.svc.hub == nil || !m.svc.hub.runtimeWanted.Load() {
		return
	}
	now := m.now()
	var ups []runtimeUpdate
	m.recMu.Lock()
	m.mu.Lock()
	fleets := make([]*fleetSync, 0, len(m.fleets))
	for _, fs := range m.fleets {
		fleets = append(fleets, fs)
	}
	m.mu.Unlock()
	seen := map[string]bool{}
	for _, fs := range fleets {
		st := fs.hub.Status()
		if now.Sub(fs.conflictsAt) >= shareSyncInterval {
			if all, err := fs.hub.Conflicts(); err == nil {
				fs.conflicts = len(all)
			}
			fs.conflictsAt = now
		}
		for _, iw := range fs.insts {
			key := runtimeKey(fs.name, iw.name)
			seen[key] = true
			var s *fleetgrpc.ShareSyncStatus
			if iw.running {
				s = m.instanceStatus(fs, st, iw, now)
			}
			if prev, ok := m.published[key]; ok && proto.Equal(prev, s) {
				continue
			}
			m.published[key] = s
			ups = append(ups, runtimeUpdate{fs.name, iw.name, shareSyncApply(s)})
		}
	}
	for key := range m.published {
		if seen[key] {
			continue
		}
		delete(m.published, key)
		f, inst, _ := strings.Cut(key, "/")
		ups = append(ups, runtimeUpdate{f, inst, shareSyncApply(nil)})
	}
	m.recMu.Unlock()
	if len(ups) > 0 {
		m.svc.hub.post(func(h *hub) { h.applyRuntime(ups) })
	}
}

func shareSyncApply(s *fleetgrpc.ShareSyncStatus) func(*fleetgrpc.InstanceRuntime) {
	return func(r *fleetgrpc.InstanceRuntime) { r.ShareSync = proto.Clone(s).(*fleetgrpc.ShareSyncStatus) }
}

// instanceStatus is a running instance's share sync as its row shows it:
// paused (why: a root over its cap or not synced, its share or the daemon
// host full), syncing (changes still to go either way, or its share's spoke
// not connected), or in sync. Called with recMu held.
func (m *shareSync) instanceStatus(fs *fleetSync, st sharesync.Status, iw instWant, now time.Time) *fleetgrpc.ShareSyncStatus {
	out := &fleetgrpc.ShareSyncStatus{Conflicts: int32(fs.conflicts)}
	var reasons []string
	add := func(r string) {
		if r != "" && !slices.Contains(reasons, r) {
			reasons = append(reasons, r)
		}
	}
	add(st.HostPaused)
	pending := 0
	for _, r := range st.Roots {
		add(r.Paused)
		add(r.Broken)
		for _, side := range r.Sides {
			if side.Side == iw.side || side.Side == shareHostSide {
				add(side.Paused)
				pending += side.Pending
			}
		}
	}
	out.Pending = int32(pending)
	run := fs.spokes[iw.side]
	attached := run != nil && run.attached.Load() && !run.ended()
	switch {
	case len(reasons) > 0:
		out.State, out.Reason = fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_PAUSED, strings.Join(reasons, "; ")
	case !attached:
		out.State, out.Reason = fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_SYNCING, "connecting to the fleet's share"
	case pending > 0:
		out.State = fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_SYNCING
	default:
		out.State = fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_IN_SYNC
		if !fs.inSync[iw.side] {
			fs.syncedAt[iw.side] = now
		}
	}
	fs.inSync[iw.side] = out.State == fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_IN_SYNC
	if at, ok := fs.syncedAt[iw.side]; ok {
		out.LastSyncedAt = timestamppb.New(at.Truncate(time.Second))
	}
	return out
}
