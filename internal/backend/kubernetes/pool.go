//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"reflect"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kclient "k8s.io/client-go/kubernetes"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/podspec"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/podseed"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
)

// Pool defaults.
const (
	// DefaultMaxBootstraps is how many pods of one connection bootstrap at
	// once.
	DefaultMaxBootstraps = 4
	// DefaultBackoffBase and DefaultBackoffCap bound the full-jitter wait
	// after a pod's failed bootstrap.
	DefaultBackoffBase = time.Second
	DefaultBackoffCap  = 30 * time.Second
	// DefaultHandshakeTimeout bounds a whole bootstrap: verification, probe,
	// stage and the agent's handshake.
	DefaultHandshakeTimeout = 2 * time.Minute
	// DefaultBulkIdle is how long an idle bulk session stays open.
	DefaultBulkIdle = 60 * time.Second
	// DefaultFallbackRate and DefaultFallbackBurst pace the fallback execs of
	// one connection (per second).
	DefaultFallbackRate  = 2.0
	DefaultFallbackBurst = 10

	// recordsRefresh is how often a lookup of an unknown connection may
	// re-read the records, recordsRecheck how often any lookup does (so a
	// changed record is picked up even when nobody calls Configure), and
	// stampCheck how often a connection's kubeconfig files are checked for a
	// change.
	recordsRefresh = time.Second
	recordsRecheck = 10 * time.Second
	stampCheck     = 2 * time.Second
)

// ErrClusterNotFound is a connection id no record names.
var ErrClusterNotFound = errors.New("no such Kubernetes connection")

// PodLayout is where fleet lives in an instance pod.
type PodLayout struct {
	// InstallDir is /opt/fleet (the PVC's fleet/), BinDir its bin/.
	InstallDir string
	BinDir     string
}

// DefaultLayout is an instance pod's.
var DefaultLayout = PodLayout{InstallDir: podseed.InstallDir, BinDir: podseed.BinDir}

// Binary is the pod's current fleet binary (the link to the content-addressed
// file).
func (l PodLayout) Binary() string { return l.BinDir + "/fleet" }

// ExecFunc runs one exec in a connection's cluster.
type ExecFunc func(ctx context.Context, c *Cluster, req kubeexec.ExecRequest, stdio kubeexec.Stdio) error

// Objects reads an instance's objects: API server Gets by default; an
// informer's listers once those run.
type Objects interface {
	Pod(ctx context.Context, namespace, name string) (*corev1.Pod, error)
	StatefulSet(ctx context.Context, namespace, name string) (*appsv1.StatefulSet, error)
}

// PoolOptions configure a Pool. Zero fields take the defaults.
type PoolOptions struct {
	// Records lists the connections (state.LoadKubeClusters).
	Records func() ([]fleet.KubeCluster, error)
	// Environ is the daemon's environment for credential plugins
	// (os.Environ).
	Environ func() []string
	// Bins finds the pod artifacts (binstore.Default).
	Bins *binstore.Store
	// SpecHash is the hash verification recomputes (podspec.SpecHash, the
	// one fleetd's render records).
	SpecHash func(*corev1.PodSpec) string
	// Exec runs execs (the connection's kube exec); Clientset is a
	// connection's client (its own); Objects reads its objects.
	Exec      ExecFunc
	Clientset func(*Cluster) (kclient.Interface, error)
	Objects   func(*Cluster) (Objects, error)
	// Logger is fleet.log (flog).
	Logger *slog.Logger
	// Layout is the pods' (DefaultLayout).
	Layout PodLayout
	// Version is the daemon's build for the agent handshake.
	Version string

	MaxBootstraps           int
	BackoffBase, BackoffCap time.Duration
	HandshakeTimeout        time.Duration
	BulkIdle                time.Duration
	FallbackRate            float64
	FallbackBurst           int

	// Now and Rand are the clock and the jitter source (tests).
	Now  func() time.Time
	Rand func() float64
}

func (o PoolOptions) withDefaults() PoolOptions {
	if o.Records == nil {
		o.Records = func() ([]fleet.KubeCluster, error) {
			k, err := state.LoadKubeClusters()
			if err != nil {
				return nil, err
			}
			return k.Clusters, nil
		}
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.SpecHash == nil {
		o.SpecHash = podspec.SpecHash
	}
	if o.Logger == nil {
		o.Logger = flog.L()
	}
	if o.Layout == (PodLayout{}) {
		o.Layout = DefaultLayout
	}
	if o.Version == "" {
		o.Version = version.Version
	}
	if o.MaxBootstraps <= 0 {
		o.MaxBootstraps = DefaultMaxBootstraps
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = DefaultBackoffBase
	}
	if o.BackoffCap <= 0 {
		o.BackoffCap = DefaultBackoffCap
	}
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if o.BulkIdle <= 0 {
		o.BulkIdle = DefaultBulkIdle
	}
	if o.FallbackRate <= 0 {
		o.FallbackRate = DefaultFallbackRate
	}
	if o.FallbackBurst <= 0 {
		o.FallbackBurst = DefaultFallbackBurst
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.Float64
	}
	return o
}

// Pool is fleetd's side of every Kubernetes connection and of the pods on
// them (see the package doc). It is safe for concurrent use.
type Pool struct {
	opts   PoolOptions
	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	clusters    map[string]*Cluster
	gates       map[string]*clusterGates
	refreshedAt time.Time
	slots       map[podKey]*podSlot
	closed      bool
}

// clusterGates are a connection's limits, kept across reloads of its
// client so in-flight bootstraps stay counted.
type clusterGates struct {
	boot     *limiter
	fallback *tokenBucket
}

// Cluster is one connection's client side. Record is the record it was
// loaded from (a record change that does not touch how the cluster is
// reached keeps it).
type Cluster struct {
	Record fleet.KubeCluster
	Conn   *kubeexec.Connection

	issuer   *kubeexec.Issuer
	gates    *clusterGates
	stamps   []fileStamp
	loadErr  error
	loadedAt time.Time
	checked  time.Time

	// client is the pool's own client of the connection (its API warnings
	// go to fleet.log), built on first use.
	clientOnce sync.Once
	client     kclient.Interface
	clientErr  error
}

// Namespace is the connection's namespace.
func (c *Cluster) Namespace() string {
	if c.Conn != nil && c.Conn.Namespace != "" {
		return c.Conn.Namespace
	}
	if c.Record.Namespace != "" {
		return c.Record.Namespace
	}
	return metav1.NamespaceDefault
}

type fileStamp struct {
	path  string
	size  int64
	mtime time.Time
	ok    bool
}

func stampFiles(paths []string) []fileStamp {
	out := make([]fileStamp, len(paths))
	for i, p := range paths {
		out[i].path = p
		if fi, err := os.Stat(p); err == nil {
			out[i] = fileStamp{path: p, size: fi.Size(), mtime: fi.ModTime(), ok: true}
		}
	}
	return out
}

var (
	defaultPoolOnce sync.Once
	defaultPool     *Pool
)

// DefaultPool is the daemon's pool. It does nothing until first used: no
// cluster is contacted at startup.
func DefaultPool() *Pool {
	defaultPoolOnce.Do(func() { defaultPool = NewPool(PoolOptions{}) })
	return defaultPool
}

// NewPool makes a pool.
func NewPool(opts PoolOptions) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	return &Pool{
		opts:     opts.withDefaults(),
		ctx:      ctx,
		cancel:   cancel,
		clusters: map[string]*Cluster{},
		gates:    map[string]*clusterGates{},
		slots:    map[podKey]*podSlot{},
	}
}

func (p *Pool) bins() *binstore.Store {
	if p.opts.Bins != nil {
		return p.opts.Bins
	}
	return binstore.Default()
}

// Configure replaces the pool's connections with records: a connection
// whose record changed in what the client depends on is reloaded (its
// sessions keep running), a removed one is dropped with its sessions.
// Callers that change a record call it; a lookup of an unknown connection
// also re-reads the records.
func (p *Pool) Configure(records []fleet.KubeCluster) {
	p.mu.Lock()
	keep := map[string]bool{}
	for _, rec := range records {
		keep[rec.ID] = true
		if c, ok := p.clusters[rec.ID]; ok && sameConnection(c.Record, rec) {
			// Only bookkeeping or settings the client does not read changed.
			continue
		}
		p.clusters[rec.ID] = p.loadLocked(rec)
	}
	var dropped []*podSlot
	for id := range p.clusters {
		if keep[id] {
			continue
		}
		delete(p.clusters, id)
		delete(p.gates, id)
		for key, slot := range p.slots {
			if key.cluster == id {
				dropped = append(dropped, slot)
				delete(p.slots, key)
			}
		}
	}
	p.refreshedAt = p.opts.Now()
	p.mu.Unlock()
	for _, slot := range dropped {
		slot.forget()
	}
}

// sameConnection reports whether a and b reach the cluster the same way.
func sameConnection(a, b fleet.KubeCluster) bool {
	return a.ID == b.ID && a.Context == b.Context && a.Namespace == b.Namespace &&
		reflect.DeepEqual(a.KubeconfigPaths, b.KubeconfigPaths) &&
		reflect.DeepEqual(a.ExtraPath, b.ExtraPath) && reflect.DeepEqual(a.PluginEnv, b.PluginEnv)
}

// loadLocked resolves rec into a fresh client side.
func (p *Pool) loadLocked(rec fleet.KubeCluster) *Cluster {
	g := p.gates[rec.ID]
	if g == nil {
		g = &clusterGates{
			boot:     newLimiter(p.opts.MaxBootstraps),
			fallback: newTokenBucket(p.opts.FallbackRate, p.opts.FallbackBurst, p.opts.Now),
		}
		p.gates[rec.ID] = g
	}
	now := p.opts.Now()
	c := &Cluster{Record: rec, gates: g, stamps: stampFiles(rec.KubeconfigPaths), loadedAt: now, checked: now}
	conn, err := kubeexec.Load(rec, kubeexec.Options{Environ: p.opts.Environ()})
	if err != nil {
		c.loadErr = fmt.Errorf("connection %s: %w", rec.ID, err)
		return c
	}
	c.Conn, c.issuer = conn, kubeexec.NewIssuer(conn)
	return c
}

// Cluster is the client side of connection id, loaded on first use and
// reloaded when its kubeconfig files change.
func (p *Pool) Cluster(id string) (*Cluster, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("the Kubernetes pool is closed")
	}
	c, ok := p.clusters[id]
	age := p.opts.Now().Sub(p.refreshedAt)
	stale := (!ok && age >= recordsRefresh) || age >= recordsRecheck
	p.mu.Unlock()
	if stale {
		records, err := p.opts.Records()
		if err != nil && !ok {
			return nil, fmt.Errorf("reading the Kubernetes connections: %w", err)
		}
		if err == nil {
			p.Configure(records)
		}
		p.mu.Lock()
		c, ok = p.clusters[id]
		p.mu.Unlock()
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrClusterNotFound, id)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.opts.Now()
	if now.Sub(c.checked) >= stampCheck {
		c.checked = now
		if !reflect.DeepEqual(stampFiles(c.Record.KubeconfigPaths), c.stamps) || c.loadErr != nil {
			c = p.loadLocked(c.Record)
			p.clusters[id] = c
		}
	}
	if c.loadErr != nil {
		return nil, c.loadErr
	}
	return c, nil
}

// IssueExecCredential is the transport the daemon-host kube-exec shim
// dials connection id with (see kubeexec.Issuer).
func (p *Pool) IssueExecCredential(ctx context.Context, id string, refresh bool) (fleet.KubeExecCredential, error) {
	c, err := p.Cluster(id)
	if err != nil {
		return fleet.KubeExecCredential{}, err
	}
	return c.issuer.Issue(ctx, refresh)
}

func (p *Pool) clientset(c *Cluster) (kclient.Interface, error) {
	if p.opts.Clientset != nil {
		return p.opts.Clientset(c)
	}
	c.clientOnce.Do(func() {
		cfg := c.Conn.RESTConfig()
		// API warnings (a PSA "would violate", a deprecated field) go to
		// fleet.log at debug, not to fleetd's stderr through klog.
		cfg.WarningHandler = warningLog{p.opts.Logger.With("cluster", c.Record.ID)}
		c.client, c.clientErr = kclient.NewForConfig(cfg)
	})
	return c.client, c.clientErr
}

// warningLog writes the API server's warnings to fleet.log.
type warningLog struct{ logger *slog.Logger }

func (w warningLog) HandleWarningHeader(code int, agent, text string) {
	if code == 299 && text != "" {
		w.logger.Debug("kube api warning", "warning", text)
	}
}

func (p *Pool) objects(c *Cluster) (Objects, error) {
	if p.opts.Objects != nil {
		return p.opts.Objects(c)
	}
	cs, err := p.clientset(c)
	if err != nil {
		return nil, err
	}
	return apiObjects{cs}, nil
}

func (p *Pool) exec(ctx context.Context, c *Cluster, req kubeexec.ExecRequest, stdio kubeexec.Stdio) error {
	if req.Namespace == "" {
		req.Namespace = c.Namespace()
	}
	if p.opts.Exec != nil {
		return p.opts.Exec(ctx, c, req, stdio)
	}
	return c.Conn.Exec(ctx, req, stdio)
}

// apiObjects reads objects with API server Gets.
type apiObjects struct{ cs kclient.Interface }

func (a apiObjects) Pod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	return a.cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
}

func (a apiObjects) StatefulSet(ctx context.Context, ns, name string) (*appsv1.StatefulSet, error) {
	return a.cs.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
}

// Close ends every session at once, without waiting on any agent, and the
// pool with them.
func (p *Pool) Close() {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Shutdown(ctx)
}

// Shutdown tells every pod's agent and bulk sessions to close and waits,
// within ctx, for them to end (the bounded shutdown's "close the sessions"
// step), then closes whatever is left. The pool is unusable afterwards.
func (p *Pool) Shutdown(ctx context.Context) {
	p.mu.Lock()
	p.closed = true
	slots := make([]*podSlot, 0, len(p.slots))
	for key, s := range p.slots {
		slots = append(slots, s)
		delete(p.slots, key)
	}
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range slots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.shutdown(ctx)
		}()
	}
	wg.Wait()
	p.cancel()
}
