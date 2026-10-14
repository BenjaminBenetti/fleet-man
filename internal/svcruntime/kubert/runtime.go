//go:build !nok8s

package kubert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
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

// leftCluster reports, under the fleet's lock, that no record of fleetName
// names the connection any more: its last instance there was destroyed (and
// the fleet's objects there released) after the caller read the state, as a
// TUI-connect reconcile may have. Making the objects again would leave them,
// and a cache server, running for a fleet with no instance there. Every
// other caller (a create, a rebuild, a re-prepare) holds a record that names
// the connection.
func (r *Runtime) leftCluster(fleetName, what string) bool {
	if fleetUsesCluster(fleetName, r.cluster) {
		return false
	}
	flog.Info("kube caches: no instance of the fleet on the connection; nothing made", "cluster", r.cluster, "fleet", fleetName, "op", what)
	return true
}

// conn is the connection as the runtime acts on it: the pool's scope (its
// client, namespace, owner label and the owner check before deletions), its
// current record, and the daemon's id, which the fleets' h8s hash.
type conn struct {
	cluster string
	scope   kubernetes.Scope
	rec     fleet.KubeCluster
	daemon  string

	// The connection's last preflight's findings, once read (capabilities);
	// factsKnown says there were some to read.
	factsRead, factsKnown bool
	caps                  *fleetgrpc.KubeCapabilities
	local                 bool
}

// factsWait bounds a wait for the connection's first preflight, when none
// has finished yet (the daemon just started).
var factsWait = 10 * time.Second

// capabilities are the connection's last preflight's findings and whether it
// is a local cluster (nil and false when there are none to read).
func (c *conn) capabilities(ctx context.Context) (*fleetgrpc.KubeCapabilities, bool) {
	if !c.factsRead {
		c.factsRead = true
		fctx, cancel := context.WithTimeout(ctx, factsWait)
		defer cancel()
		if f, err := connectionFacts(fctx, c.cluster); err == nil {
			c.factsKnown = true
			c.caps = f.Capabilities
			c.local = f.Local.Local() || f.Capabilities.GetLocalCluster()
		}
	}
	return c.caps, c.local
}

// buildkitRootful reports whether the fleet's shared Buildkit runs rootful on
// the connection: where it allows privileged pods (auto on a local cluster).
// Elsewhere it runs rootless. While no preflight has said whether the
// connection is local (the daemon just started), a server already there
// keeps the shape it has: a guess would replace its pod, and the other shape
// writes the same cache volume as another user.
func (c *conn) buildkitRootful(ctx context.Context, m fleetMeta) bool {
	_, local := c.capabilities(ctx)
	if c.factsKnown || c.rec.AllowPrivileged != nil {
		return c.rec.AllowsPrivileged(local)
	}
	dep, err := c.scope.Client.AppsV1().Deployments(m.Namespace).Get(ctx, m.name(names.SuffixBuildkit), metav1.GetOptions{})
	if err != nil {
		return false
	}
	return deploymentRootful(dep)
}

// deploymentRootful reports whether dep runs the shared Buildkit server
// rootful (privileged).
func deploymentRootful(dep *appsv1.Deployment) bool {
	for _, ctr := range dep.Spec.Template.Spec.Containers {
		if ctr.Name == string(fleet.ServiceBuildkit) {
			sc := ctr.SecurityContext
			return sc != nil && sc.Privileged != nil && *sc.Privileged
		}
	}
	return false
}

// annotationBuildkitShape on Buildkit's cache volume is the shape of the
// server whose cache it holds (buildkitShape; keepBuildkitShape).
const annotationBuildkitShape = "fleet.dev/buildkit-shape"

// buildkitShape names a Buildkit server's shape.
func buildkitShape(rootful bool) string {
	if rootful {
		return "rootful"
	}
	return "rootless"
}

// BuildkitRefusedError is a connection that does not run the fleets' shared
// Buildkit: its Allow shared Buildkit setting is off, or its namespace's
// admission denies the server's pod (as its last preflight found). The
// fleets' instances there build without it: it is
// fleet.ErrNoServiceEndpoint.
type BuildkitRefusedError struct{ Why string }

func (e *BuildkitRefusedError) Error() string { return e.Why }

func (e *BuildkitRefusedError) Is(target error) bool { return target == fleet.ErrNoServiceEndpoint }

// buildkitNotAllowed is the refusal of a connection whose Allow shared
// Buildkit setting is off.
func buildkitNotAllowed(rec *fleet.KubeCluster) error {
	return &BuildkitRefusedError{Why: fmt.Sprintf("shared Buildkit is not allowed on %s (its Allow shared Buildkit setting, allow_shared_buildkit, is off): the fleet's instances there build without it", rec.GetDisplayName())}
}

// buildkitRefusal is why the connection does not run the fleets' shared
// Buildkit (nil when it does).
func (c *conn) buildkitRefusal(ctx context.Context) error {
	if !c.rec.AllowsSharedBuildkit() {
		return buildkitNotAllowed(&c.rec)
	}
	if caps, _ := c.capabilities(ctx); caps != nil && !caps.GetBuildkitAdmitted() && caps.GetBuildkitReason() != "" {
		return &BuildkitRefusedError{Why: caps.GetBuildkitReason()}
	}
	return nil
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
// connection (footprint.json), voiding a teardown owed there. It makes
// nothing for a fleet no record names on the connection (see leftCluster).
func (r *Runtime) PrepareFleet(fleetName string) error {
	unlock := r.lock(fleetName)
	defer unlock()
	if r.leftCluster(fleetName, "prepare") {
		return nil
	}
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
// says how it went. Like PrepareFleet, it makes nothing for a fleet no
// record names on the connection. Shared Buildkit also gets its TLS Secret
// (tls.go); on a connection that does not run it (BuildkitRefusedError) a
// server left from before goes (its volume stays) and the refusal comes
// back.
func (r *Runtime) EnsureService(fleetName string, svc fleet.SharedService) error {
	if _, err := lookup(svc); err != nil {
		return err
	}
	unlock := r.lock(fleetName)
	defer unlock()
	if r.leftCluster(fleetName, "ensure "+string(svc)) {
		return nil
	}
	c, err := r.connect()
	if err != nil {
		return err
	}
	ctx, cancel := opContext()
	defer cancel()
	m := c.meta(fleetName)
	if svc == fleet.ServiceBuildkit {
		if refused := c.buildkitRefusal(ctx); refused != nil {
			c.stopRefusedBuildkit(ctx, m)
			return refused
		}
	}
	if err := c.ensureService(ctx, m, svc); err != nil {
		return fmt.Errorf("the %s on %s: %w", serviceText(svc), c.rec.GetDisplayName(), err)
	}
	watchReady(c, m, svc)
	return nil
}

// stopRefusedBuildkit removes a shared Buildkit server the connection no
// longer runs (its setting turned off, or admission now denying it),
// keeping its volume. Best-effort, logged.
func (c *conn) stopRefusedBuildkit(ctx context.Context, m fleetMeta) {
	forgetReady(c.cluster, m, fleet.ServiceBuildkit)
	td, err := c.scope.DeleteSelected(ctx, m.serviceSelector(fleet.ServiceBuildkit), serverKinds)
	if err != nil {
		flog.Warn("kube buildkit: removing a server the connection does not run", "cluster", c.cluster, "fleet", m.Fleet, "err", err)
		return
	}
	if len(td.Deleted) > 0 {
		flog.Info("kube buildkit: removed a server the connection does not run (its volume stays)", "cluster", c.cluster, "fleet", m.Fleet, "deleted", len(td.Deleted))
	}
}

// InstanceMounts is nil: a pod reaches its fleet's services over the
// network.
func (r *Runtime) InstanceMounts(string, fleet.SharedService) []backend.Mount { return nil }

// Endpoint is svc's ClusterIP Service, by the short name the fleet's pods
// resolve in their own namespace (their search domain), as docker's
// instances reach a cache by its container name: the deb cache's proxy on
// 3142, the image cache's mirror on 5000. Its server comes up on the
// cluster's time after an ensure (watchReady) and no start revives it, so
// the endpoint is a Fallback one: apt checks it answers first, and dockerd
// falls back from a mirror that does not by itself. Shared Buildkit is its
// Service's TCP port, which takes only mutual TLS (each instance gets its
// certificate from IssueCredential); a connection whose Allow shared
// Buildkit is off answers BuildkitRefusedError. It reads only the daemon's
// id, which the fleet's object names hash, and the connection's record.
func (r *Runtime) Endpoint(fleetName string, svc fleet.SharedService) (fleet.ServiceEndpoint, error) {
	s, err := lookup(svc)
	if err != nil {
		return fleet.ServiceEndpoint{}, err
	}
	if svc == fleet.ServiceBuildkit {
		rec, err := pool().Record(r.cluster)
		if err != nil {
			return fleet.ServiceEndpoint{}, err
		}
		if !rec.AllowsSharedBuildkit() {
			return fleet.ServiceEndpoint{}, buildkitNotAllowed(&rec)
		}
	}
	id, err := daemonID()
	if err != nil {
		return fleet.ServiceEndpoint{}, fmt.Errorf("the daemon's identity: %w", err)
	}
	host := names.FleetObject(fleetName, names.FleetHash(id, r.cluster, fleetName), s.suffix)
	hostPort := net.JoinHostPort(host, strconv.Itoa(int(s.port)))
	if svc == fleet.ServiceBuildkit {
		return fleet.ServiceEndpoint{URL: "tcp://" + hostPort, HostPort: hostPort}, nil
	}
	return fleet.ServiceEndpoint{URL: "http://" + hostPort, HostPort: hostPort, Fallback: true}, nil
}

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
// returns once the server is up (within wipeTimeout). Shared Buildkit on a
// connection that does not run it only loses a volume left from before.
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
