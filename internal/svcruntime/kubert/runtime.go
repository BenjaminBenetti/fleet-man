//go:build !nok8s

package kubert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// KeyPrefix starts a connection runtime's Key: "kubernetes:<connection id>".
const KeyPrefix = "kubernetes:"

// Key is connection clusterID's runtime Key.
func Key(clusterID string) string { return KeyPrefix + clusterID }

// Bounds of the runtime's work on a cluster; variables so tests shorten
// them.
var (
	// opTimeout bounds a prepare, an ensure, a stop and a teardown.
	opTimeout = 2 * time.Minute
	// wipeTimeout bounds a cache wipe, which waits for the empty server to
	// be back (the clients' Delete cache deadline is 300s).
	wipeTimeout = 270 * time.Second
	// readyTimeout bounds watchReady's watch of a server coming up.
	readyTimeout = 10 * time.Minute
	// goneTimeout bounds a wait for an object being deleted to be gone.
	goneTimeout = 60 * time.Second
	// pollInterval paces the waits.
	pollInterval = 2 * time.Second
	// reconcileBudget is a TUI-connect sweep's bound on one connection.
	reconcileBudget = 2 * time.Minute
)

// access is what the runtime uses of the daemon's Kubernetes pool
// (*kubernetes.Pool).
type access interface {
	Scope(clusterID string) (kubernetes.Scope, error)
	Record(clusterID string) (fleet.KubeCluster, error)
	ExecService(ctx context.Context, clusterID, pod, container string, argv []string, stdout, stderr io.Writer) (int, error)
}

// The runtime's seams; package vars so tests substitute fakes.
var (
	pool     = func() access { return kubernetes.DefaultPool() }
	daemonID = func() (string, error) {
		id, err := owner.LoadIdentity()
		return id.DaemonID, err
	}
	connectionFacts = kubernetes.ConnectionFacts
	now             = time.Now
)

// Hooks are what the daemon tells the runtimes about its connections.
type Hooks struct {
	// Usable reports whether connection clusterID was last judged usable
	// (ready or limited). Nil: every connection is.
	Usable func(clusterID string) bool
}

var (
	hooksMu sync.Mutex
	hooks   Hooks
)

// SetHooks installs the daemon's hooks (fleetd, at start).
func SetHooks(h Hooks) {
	hooksMu.Lock()
	hooks = h
	hooksMu.Unlock()
}

func usable(clusterID string) bool {
	hooksMu.Lock()
	u := hooks.Usable
	hooksMu.Unlock()
	return u == nil || u(clusterID)
}

// fleetUsesCluster reports whether a record of fleetName names connection
// clusterID, whatever its status (a create in flight, a destroy not done);
// an unreadable state.json counts as yes, so nothing is torn down on a
// guess. A package var for tests.
var fleetUsesCluster = func(fleetName, clusterID string) bool {
	st, err := state.Load()
	if err != nil {
		return true
	}
	f, ok := st.Fleets[fleetName]
	if !ok || f == nil {
		return false
	}
	for _, inst := range f.Instances {
		if inst != nil && inst.Backend == fleet.BackendKubernetes && inst.KubeCluster == clusterID {
			return true
		}
	}
	return false
}

// fleetExists reports whether state.json still has fleetName (unreadable:
// yes).
var fleetExists = func(fleetName string) bool {
	st, err := state.Load()
	if err != nil {
		return true
	}
	_, ok := st.Fleets[fleetName]
	return ok
}

// Runtime is the shared-service runtime of one connection. Its zero value
// names no connection; For makes one.
type Runtime struct {
	cluster string
}

// For is connection clusterID's runtime. No I/O.
func For(clusterID string) *Runtime { return &Runtime{cluster: clusterID} }

// Cluster is the connection's record id.
func (r *Runtime) Cluster() string { return r.cluster }

// Key is Key(the connection).
func (r *Runtime) Key() string { return Key(r.cluster) }

// fleetLocks serializes the runtime's work on one fleet on one connection.
var fleetLocks sync.Map // "<cluster>/<fleet>" -> *sync.Mutex

func (r *Runtime) lock(fleetName string) func() {
	v, _ := fleetLocks.LoadOrStore(r.cluster+"/"+fleetName, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// conn is the connection as the runtime acts on it: the pool's scope (its
// client, namespace, owner label and the owner check before deletions), its
// current record, and the daemon's id, which the fleets' h8s hash.
type conn struct {
	cluster string
	scope   kubernetes.Scope
	rec     fleet.KubeCluster
	daemon  string
}

func (r *Runtime) connect() (*conn, error) {
	if r.cluster == "" {
		return nil, errors.New("kubernetes runtime: no connection")
	}
	p := pool()
	s, err := p.Scope(r.cluster)
	if err != nil {
		return nil, err
	}
	rec, err := p.Record(r.cluster)
	if err != nil {
		return nil, err
	}
	id, err := daemonID()
	if err != nil {
		return nil, fmt.Errorf("the daemon's identity: %w", err)
	}
	return &conn{cluster: r.cluster, scope: s, rec: rec, daemon: id}, nil
}

// meta names fleetName's objects on the connection.
func (c *conn) meta(fleetName string) fleetMeta {
	return fleetMeta{
		Namespace: c.scope.Namespace, Owner: c.scope.Owner, Cluster: c.cluster,
		Fleet: fleetName, Hash: names.FleetHash(c.daemon, c.cluster, fleetName),
	}
}

func opContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), opTimeout)
}

// PrepareFleet applies fleetName's anchor, its headless Service and its
// NetworkPolicies (the egress block only while the connection blocks
// metadata; turned off, the policy goes), and records the fleet on the
// connection (footprint.json), voiding a teardown owed there.
func (r *Runtime) PrepareFleet(fleetName string) error {
	unlock := r.lock(fleetName)
	defer unlock()
	c, err := r.connect()
	if err != nil {
		return err
	}
	ctx, cancel := opContext()
	defer cancel()
	m := c.meta(fleetName)
	if err := c.prepare(ctx, m); err != nil {
		return fmt.Errorf("the fleet's network on %s: %w", c.rec.GetDisplayName(), err)
	}
	if err := kubernetes.RecordFleetPrepared(c.cluster, fleetName, m.Hash, now()); err != nil {
		return fmt.Errorf("recording the fleet on %s: %w", c.rec.GetDisplayName(), err)
	}
	return nil
}

// EnsureService applies svc's volume, Deployment and Service for fleetName
// (and the anchor they hang off, when it is missing) and returns without
// waiting for the server to be up: watchReady follows it, and CacheUsage
// says how it went. Shared Buildkit is not served on a cluster yet: it needs
// per-instance TLS.
func (r *Runtime) EnsureService(fleetName string, svc fleet.SharedService) error {
	if _, err := lookup(svc); err != nil {
		return err
	}
	if svc == fleet.ServiceBuildkit {
		return errBuildkitNotServed
	}
	unlock := r.lock(fleetName)
	defer unlock()
	c, err := r.connect()
	if err != nil {
		return err
	}
	ctx, cancel := opContext()
	defer cancel()
	m := c.meta(fleetName)
	if err := c.ensureService(ctx, m, svc); err != nil {
		return fmt.Errorf("the %s on %s: %w", serviceText(svc), c.rec.GetDisplayName(), err)
	}
	watchReady(c, m, svc)
	return nil
}

// errBuildkitNotServed refuses shared Buildkit on a cluster: an instance
// reaches it over the network, which takes a per-instance client
// certificate fleet does not issue yet.
var errBuildkitNotServed = errors.New("shared Buildkit does not run on a cluster yet: it needs a per-instance client certificate, which this version of fleet does not issue; builds use the instance's own builder")

// InstanceMounts is nil: a pod reaches its fleet's services over the
// network.
func (r *Runtime) InstanceMounts(string, fleet.SharedService) []backend.Mount { return nil }

// Endpoint answers ErrNoEndpoint: the services run, but this version of
// fleet does not point a cluster's instances at them (their apt and docker
// go direct).
func (r *Runtime) Endpoint(_ string, svc fleet.SharedService) (fleet.ServiceEndpoint, error) {
	if _, err := lookup(svc); err != nil {
		return fleet.ServiceEndpoint{}, err
	}
	return fleet.ServiceEndpoint{}, noEndpoint{svc}
}

// noEndpoint is a service the connection runs but does not point its
// instances at (fleet.ErrNoServiceEndpoint).
type noEndpoint struct{ svc fleet.SharedService }

func (e noEndpoint) Error() string {
	if e.svc == fleet.ServiceBuildkit {
		return errBuildkitNotServed.Error()
	}
	return fmt.Sprintf("the fleet's %s runs on the cluster, but this version of fleet does not point the cluster's instances at it: they go direct", serviceText(e.svc))
}

func (e noEndpoint) Is(target error) bool { return target == fleet.ErrNoServiceEndpoint }

// ConnectInstance does nothing: the fleet's NetworkPolicies already admit
// every instance of the fleet to its caches.
func (r *Runtime) ConnectInstance(string, string) error { return nil }

// StopService deletes svc's Deployment and Service, keeping its volume.
func (r *Runtime) StopService(fleetName string, svc fleet.SharedService) error {
	if _, err := lookup(svc); err != nil {
		return err
	}
	unlock := r.lock(fleetName)
	defer unlock()
	c, err := r.connect()
	if err != nil {
		return err
	}
	ctx, cancel := opContext()
	defer cancel()
	m := c.meta(fleetName)
	forgetReady(r.cluster, m, svc)
	if _, err := c.scope.DeleteSelected(ctx, m.serviceSelector(svc), serverKinds); err != nil {
		return fmt.Errorf("the %s on %s: %w", serviceText(svc), c.rec.GetDisplayName(), err)
	}
	return nil
}

// WipeService empties svc's cache: its server is scaled to nothing, its
// volume deleted and made again, and the server brought back on it; it
// returns once the server is up (within wipeTimeout). Buildkit, which does
// not run on a cluster yet, only loses a volume left from before.
func (r *Runtime) WipeService(fleetName string, svc fleet.SharedService) error {
	if _, err := lookup(svc); err != nil {
		return err
	}
	unlock := r.lock(fleetName)
	defer unlock()
	c, err := r.connect()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), wipeTimeout)
	defer cancel()
	m := c.meta(fleetName)
	if err := c.wipe(ctx, m, svc); err != nil {
		return fmt.Errorf("the %s on %s: %w", serviceText(svc), c.rec.GetDisplayName(), err)
	}
	return nil
}

// ReleaseFleet tears fleetName's objects on the connection down, keeping its
// cache volumes, unless a record of the fleet still names the connection.
func (r *Runtime) ReleaseFleet(fleetName string) error {
	return r.teardown(fleetName, false)
}

// TeardownFleet tears every object of fleetName on the connection down (its
// cache volumes too where the connection deletes them with the fleet) and
// forgets the fleet there.
func (r *Runtime) TeardownFleet(fleetName string) error {
	return r.teardown(fleetName, true)
}

// ReconfigureOnStart is false: a fleet's Services and NetworkPolicies, and
// its servers, outlive its instances being stopped, so a start has nothing
// to rejoin.
func (r *Runtime) ReconfigureOnStart() bool { return false }

// Reachable reports whether the daemon last judged the connection usable.
func (r *Runtime) Reachable(context.Context) bool { return usable(r.cluster) }

// ReconcileBudget bounds a TUI-connect sweep of the connection.
func (r *Runtime) ReconcileBudget() time.Duration { return reconcileBudget }

// serviceText is how messages name svc.
func serviceText(svc fleet.SharedService) string {
	switch svc {
	case fleet.ServiceBuildkit:
		return "shared Buildkit"
	case fleet.ServiceDebCache:
		return "deb cache"
	case fleet.ServiceImageCache:
		return "image cache"
	}
	return string(svc)
}
