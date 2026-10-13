package imageserve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
)

// Tunnel is fleetd's gateway connection as gateway delivery needs it
// (internal/server/remote's Manager).
type Tunnel interface {
	// Registry returns the registry prefix the gateway assigned this daemon
	// ("<host[:port]>/<rid>"), or "" and why there is none.
	Registry() (prefix, why string)
	// WarmRegistry asks for the dedicated registry connection now: a pod is
	// about to pull. It closes again once idle for a minute.
	WarmRegistry()
}

// Connections reaches the clusters of gateway-delivered images when the
// registry prefix changes (the cluster backend implements it).
type Connections interface {
	// Target is a connection's delivery target, its Secrets bound to its
	// namespace.
	Target(ctx context.Context, clusterID string) (kubeimage.Target, error)
	// RepointImages patches the dev container image of the connection's
	// StatefulSets from each old reference in refs to its new one. They are
	// OnDelete: running pods keep theirs, and any pod Kubernetes creates
	// later pulls the new reference.
	RepointImages(ctx context.Context, t kubeimage.Target, refs map[string]string) error
}

// Index is the image index whose gateway entries a re-point rewrites
// (kubeimage.Manager).
type Index interface {
	Entries(clusterID string) ([]kubeimage.Entry, error)
	RepointGateway(clusterID string, ref func(kubeimage.Entry) string) (map[string]string, error)
}

// Gateway is gateway image delivery (kubeimage.Gateway): fleetd serves the
// images it built for a cluster through the Fleet gateway, at
// <host>/<rid>/devcontainer@<digest>, behind a per-connection credential
// kept in one long-lived managed pull Secret per connection.
type Gateway struct {
	Store    *Store
	Clusters Clusters
	Tunnel   Tunnel
	// Uplink paces uploads; nil sends them unpaced.
	Uplink *Uplink
	// Connections and Index re-point gateway-delivered references when the
	// registry id or host changes; with either nil that part waits.
	Connections Connections
	Index       Index
	// Transport carries the daemon-side route check (CheckRoute) to the
	// gateway's public URL; nil is http.DefaultTransport (the daemon host's
	// trust store).
	Transport http.RoundTripper
	// OnPull is told the connection after each step of a node's pull (its
	// manifest, then each blob), for the live pull statistics; it must not
	// block.
	OnPull func(clusterID string)

	once    sync.Once
	handler *Handler
	// repointMu serializes re-points.
	repointMu sync.Mutex
	// sweepMu guards sweepTimer, the follow-up sweep a release arms
	// (sweepSoon); sweepDelay overrides sweepAfter (tests).
	sweepMu    sync.Mutex
	sweepTimer *time.Timer
	sweepDelay time.Duration
	// retryMu guards retryTimer, the retry of a failed re-point, and
	// retryWait, its delay; retryMin overrides repointRetryMin (tests).
	retryMu    sync.Mutex
	retryTimer *time.Timer
	retryWait  time.Duration
	retryMin   time.Duration
}

// sweepAfter is when a release's follow-up sweep runs: once the grace the
// sweep keeps a just-released manifest for has passed.
const sweepAfter = sweepGrace + 5*time.Second

// repointTimeout bounds one connection's re-point, so an unreachable
// cluster cannot hold up the re-points after it.
const repointTimeout = 2 * time.Minute

// A failed re-point is retried after repointRetryMin, the wait doubling up
// to repointRetryMax: the prefix is reported only when it changes, so
// nothing else would try again.
const (
	repointRetryMin = time.Minute
	repointRetryMax = 15 * time.Minute
)

var _ kubeimage.Gateway = (*Gateway)(nil)

// Handler is the registry the gateway's TagRegistry streams are served
// with.
func (g *Gateway) Handler() *Handler {
	g.once.Do(func() { g.handler = &Handler{Store: g.Store, Clusters: g.Clusters, Uplink: g.Uplink, Notify: g.OnPull} })
	return g.handler
}

// unavailableHint is what to change when gateway delivery cannot work.
const unavailableHint = "turn on Enable Image Registry with a gateway that offers it (a --session-key and an https --public-url with no path), or set a registry in the cluster's image settings"

// Available implements kubeimage.Gateway.
func (g *Gateway) Available(kubeimage.Target) (bool, string) {
	if g.Tunnel == nil || g.Store == nil || g.Clusters == nil {
		return false, "gateway delivery is not available in this daemon"
	}
	if prefix, why := g.Tunnel.Registry(); prefix == "" {
		return false, why
	}
	return true, ""
}

func unavailable(why string) error {
	return &kubeimage.Error{Code: kubeimage.CodeNoDelivery, Err: errors.New("gateway delivery is unavailable: " + why), Hint: unavailableHint}
}

// GatewayPullSecretName is the managed pull Secret holding a connection's
// gateway credential: "fleet-gwpull-<owner12>-<cluster id>". It names the
// connection as well as the daemon, so two connections to one namespace
// each keep their own; it is separate from the registry delivery's Secret
// (kubeimage.PullSecretName), which follows the daemon host's login.
func GatewayPullSecretName(daemonID, clusterID string) string {
	return "fleet-gwpull-" + names.Owner(daemonID) + "-" + clusterID
}

// Ref is the reference a pod pulls a gateway-served manifest by.
func Ref(prefix, digest string) string { return prefix + "/" + repoName + "@" + digest }

// registryHost is the host[:port] of a registry prefix.
func registryHost(prefix string) string {
	host, _, _ := strings.Cut(prefix, "/")
	return host
}

// Deliver implements kubeimage.Gateway: the image is prepared (docker save,
// once per image and platform), the connection starts serving its
// node-platform manifest, the pull Secret is brought in line, and the
// registry connection is opened for the pull to come.
func (g *Gateway) Deliver(ctx context.Context, t kubeimage.Target, img kubeimage.Image) (kubeimage.GatewayDelivery, error) {
	if ok, why := g.Available(t); !ok {
		return kubeimage.GatewayDelivery{}, unavailable(why)
	}
	if t.Secrets == nil {
		return kubeimage.GatewayDelivery{}, errors.New("gateway delivery: no namespace to keep the pull Secret in")
	}
	cred, err := g.Clusters.EnsureCredential(t.Cluster.ID)
	if err != nil {
		return kubeimage.GatewayDelivery{}, fmt.Errorf("gateway delivery: the connection's pull credential: %w", err)
	}
	plat := t.Platform
	if plat == "" {
		plat = "linux/" + runtime.GOARCH
	}
	prep, err := g.Store.Prepare(ctx, img.Config.ID, plat)
	if err != nil {
		var gone *ImageGoneError
		if errors.As(err, &gone) {
			return kubeimage.GatewayDelivery{}, &kubeimage.Error{Code: kubeimage.CodeImagePull, Err: err, Hint: "rebuild the instance to build it again"}
		}
		if isContextErr(err) {
			return kubeimage.GatewayDelivery{}, err
		}
		return kubeimage.GatewayDelivery{}, &kubeimage.Error{Code: kubeimage.CodeImagePush, Err: err}
	}
	ref, secret, err := g.serve(ctx, t, prep.Digest, img.Config.ID, cred)
	if err != nil {
		return kubeimage.GatewayDelivery{}, err
	}
	fmt.Fprintf(backend.JobLogFrom(ctx), "image %s is served through the gateway as %s (%s to pull on a fresh node)\n", short(img.Config.ID), ref, humanBytes(prep.Bytes))
	return kubeimage.GatewayDelivery{Ref: ref, Digest: prep.Digest, Config: prep.Config, Location: "gateway", PullSecret: secret}, nil
}

// serve makes the connection serve a cached manifest for holder: the pull
// Secret is brought in line (when the target has Secrets), the prefix
// bookkeeping follows the current registry, the Store takes the reference
// and the registry connection is opened for the pull to come. It returns the
// reference and the Secret's name ("" without Secrets).
func (g *Gateway) serve(ctx context.Context, t kubeimage.Target, digest, holder, cred string) (string, string, error) {
	// The prefix is read only now: the export can take minutes, and a new
	// registry id or host meanwhile must neither be handed out stale nor
	// have this delivery re-point the connection back to the old one.
	prefix, why := g.Tunnel.Registry()
	if prefix == "" {
		return "", "", unavailable(why)
	}
	secret := ""
	if t.Secrets != nil {
		secret = GatewayPullSecretName(t.DaemonID, t.Cluster.ID)
		if err := g.applySecret(ctx, t, registryHost(prefix), cred); err != nil {
			return "", "", err
		}
	}
	switch recorded, err := g.Store.Prefix(t.Cluster.ID); {
	case err != nil:
		return "", "", err
	case recorded == "":
		if err := g.Store.SetPrefix(t.Cluster.ID, prefix); err != nil {
			return "", "", err
		}
	case recorded != prefix:
		// The connection's older references name another registry id or
		// host; this delivery already names the current one.
		g.RegistryChanged(prefix)
	}
	// The connection serves the image from here on. Nothing that can fail
	// comes after, so a failed delivery never leaves it holding an image
	// its index does not know (and kubeimage's GC would never let go).
	if err := g.Store.Ref(t.Cluster.ID, digest, holder); err != nil {
		return "", "", err
	}
	g.Tunnel.WarmRegistry()
	return Ref(prefix, digest), secret, nil
}

// Remove implements kubeimage.Gateway: the connection stops serving the
// image, and the cache drops what nothing serves any more.
func (g *Gateway) Remove(_ context.Context, t kubeimage.Target, e kubeimage.Entry) error {
	if !validDigest(e.Digest) {
		return nil
	}
	if err := g.Store.Unref(t.Cluster.ID, e.Digest, e.ImageID); err != nil {
		return err
	}
	// The collection pass must not wait out an import. The sweep keeps what
	// it just let go for sweepGrace, and a later pass sweeps only if it
	// collects something too, so a follow-up sweep takes it.
	_, _, err := g.Store.TrySweep()
	g.sweepSoon()
	return err
}

// sweepSoon arms one follow-up sweep for once the grace of what was just
// released has passed (or pushes the armed one back). One that meets an
// import is tried again later.
func (g *Gateway) sweepSoon() {
	g.sweepMu.Lock()
	defer g.sweepMu.Unlock()
	d := g.sweepDelay
	if d <= 0 {
		d = sweepAfter
	}
	if g.sweepTimer == nil {
		g.sweepTimer = time.AfterFunc(d, g.sweepNow)
		return
	}
	g.sweepTimer.Reset(d)
}

func (g *Gateway) sweepNow() {
	_, ran, err := g.Store.TrySweep()
	if err != nil {
		flog.Warn("gateway image cache sweep failed", "err", err)
	}
	if !ran {
		g.sweepSoon()
	}
}

// Warm opens the registry connection ahead of a pull fleetd starts (an
// instance's start or evacuation).
func (g *Gateway) Warm() {
	if g.Tunnel != nil {
		g.Tunnel.WarmRegistry()
	}
}

// Forget is the end of a connection (it is being removed, after its last GC
// pass): the cache stops serving it anything and the gateway pull Secret is
// deleted. The credential goes with the connection's record. Like Remove it
// never waits out an import (the removal RPC calls it): the follow-up sweep
// takes what it let go.
func (g *Gateway) Forget(ctx context.Context, t kubeimage.Target) error {
	if err := g.Store.Forget(t.Cluster.ID); err != nil {
		return err
	}
	if _, _, err := g.Store.TrySweep(); err != nil {
		return err
	}
	g.sweepSoon()
	if t.Secrets == nil {
		return nil
	}
	return t.Secrets.DeletePullSecret(ctx, GatewayPullSecretName(t.DaemonID, t.Cluster.ID))
}

// RotateCredential replaces a connection's gateway credential and rewrites
// it inside the same pull Secret: the Secret is applied, never recreated,
// because a 1.35+ kubelet re-checks every cached image whose pull Secret
// changed identity. The old credential keeps working for a few minutes.
func (g *Gateway) RotateCredential(ctx context.Context, t kubeimage.Target) error {
	cred, err := g.Clusters.RotateCredential(t.Cluster.ID)
	if err != nil {
		return err
	}
	if t.Secrets == nil || g.Tunnel == nil {
		return nil
	}
	prefix, _ := g.Tunnel.Registry()
	if prefix == "" {
		if prefix, err = g.Store.Prefix(t.Cluster.ID); err != nil || prefix == "" {
			return err
		}
	}
	return g.applySecret(ctx, t, registryHost(prefix), cred)
}

// applySecret brings the connection's gateway pull Secret in line: the
// credential for host, under the connection's labels.
func (g *Gateway) applySecret(ctx context.Context, t kubeimage.Target, host, cred string) error {
	data, err := dockerConfigJSON(host, t.Cluster.ID, cred)
	if err != nil {
		return err
	}
	s := kubeimage.PullSecret{Name: GatewayPullSecretName(t.DaemonID, t.Cluster.ID), Labels: t.Labels, Registry: host, DockerConfigJSON: data}
	if err := t.Secrets.ApplyPullSecret(ctx, s); err != nil {
		return fmt.Errorf("applying the gateway pull Secret %s: %w", s.Name, err)
	}
	return nil
}

// dockerConfigJSON is a .dockerconfigjson with one Basic login for host.
func dockerConfigJSON(host, user, pass string) ([]byte, error) {
	type entry struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Auth     string `json:"auth"`
	}
	return json.Marshal(struct {
		Auths map[string]entry `json:"auths"`
	}{Auths: map[string]entry{host: {Username: user, Password: pass, Auth: base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))}}})
}

// RegistryChanged is told the registry prefix of every registration
// (remote.WithRegistryChange). When it differs from what a connection's
// gateway-delivered references name (a new registry id after the gateway
// lost its session key, or a new gateway host), every such connection is
// re-pointed at once, in the background: its pull Secret names the new host,
// its index entries and its StatefulSets the new reference. "" (no registry
// now) changes nothing: the references stay for when it comes back.
func (g *Gateway) RegistryChanged(prefix string) {
	if prefix == "" || g.Store == nil {
		return
	}
	go g.repoint(context.Background(), prefix)
}

// repoint re-points every connection whose references name another prefix.
// A connection that fails keeps its recorded prefix and is tried again
// (retryRepoint).
func (g *Gateway) repoint(ctx context.Context, prefix string) {
	g.repointMu.Lock()
	defer g.repointMu.Unlock()
	// Passes run in no set order, and one for a prefix the tunnel has since
	// replaced would point the connections back at it. The tunnel reports
	// every change, so the pass for the current prefix does the work.
	if g.Tunnel != nil {
		if current, _ := g.Tunnel.Registry(); current != prefix {
			return
		}
	}
	failed := false
	defer func() { g.retryRepoint(prefix, failed) }()
	clusters, err := g.Store.Clusters()
	if err != nil {
		flog.Warn("gateway image re-point: reading the image cache", "err", err)
		failed = true
		return
	}
	for _, c := range clusters {
		old, err := g.Store.Prefix(c)
		if err != nil || old == prefix {
			continue
		}
		if g.servesOnlyProbes(c) {
			// Nothing in the cluster names the probe image (its pods are
			// gone after each test, and the next one serves it under the
			// current prefix): the prefix is just recorded.
			if err := g.Store.SetPrefix(c, prefix); err != nil {
				flog.Warn("gateway image re-point: recording the prefix", "cluster", c, "err", err)
				failed = true
			}
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, repointTimeout)
		err = g.repointCluster(cctx, c, old, prefix)
		cancel()
		if err != nil {
			flog.Warn("gateway image re-point failed", "cluster", c, "from", old, "to", prefix, "err", err)
			// Without cluster access there is nothing to retry with; the
			// next change or delivery tries again.
			failed = failed || !errors.Is(err, errNoConnections)
			continue
		}
		if err := g.Store.SetPrefix(c, prefix); err != nil {
			flog.Warn("gateway image re-point: recording the prefix", "cluster", c, "err", err)
			failed = true
		}
	}
}

// retryRepoint schedules another pass for prefix after a failed one, with
// backoff, or ends the retries after one that did everything. A retry the
// prefix has outdated finds nothing to do (repoint's check).
func (g *Gateway) retryRepoint(prefix string, failed bool) {
	g.retryMu.Lock()
	defer g.retryMu.Unlock()
	if g.retryTimer != nil {
		g.retryTimer.Stop()
		g.retryTimer = nil
	}
	if !failed {
		g.retryWait = 0
		return
	}
	lo := g.retryMin
	if lo <= 0 {
		lo = repointRetryMin
	}
	g.retryWait = min(max(2*g.retryWait, lo), repointRetryMax)
	g.retryTimer = time.AfterFunc(g.retryWait, func() { g.repoint(context.Background(), prefix) })
}

// servesOnlyProbes reports a connection that serves nothing but the probe
// image (Probe). It goes by the holder the Store records, not by the probe
// digests this process has built: after a restart none is built yet.
func (g *Gateway) servesOnlyProbes(clusterID string) bool {
	return g.Store.heldOnlyBy(clusterID, probeHolder)
}

// errNoConnections: the daemon cannot reach the clusters yet.
var errNoConnections = errors.New("no cluster access to re-point with")

func (g *Gateway) repointCluster(ctx context.Context, clusterID, old, prefix string) error {
	if g.Connections == nil {
		return errNoConnections
	}
	t, err := g.Connections.Target(ctx, clusterID)
	if err != nil {
		return err
	}
	if t.Secrets != nil && g.Clusters != nil {
		cred, err := g.Clusters.EnsureCredential(clusterID)
		if err != nil {
			return err
		}
		if err := g.applySecret(ctx, t, registryHost(prefix), cred); err != nil {
			return err
		}
	}
	// Every reference a StatefulSet may still name: what the index holds,
	// and what the last re-pointed prefix made (an earlier failed pass may
	// have left StatefulSets on it). The index is rewritten only once the
	// StatefulSets are, so a failed pass is redone in full.
	refs := map[string]string{}
	if old != "" {
		for _, d := range g.Store.servedManifests(clusterID) {
			refs[Ref(old, d)] = Ref(prefix, d)
		}
	}
	if g.Index != nil {
		entries, err := g.Index.Entries(clusterID)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Strategy == kubeimage.StrategyGateway && validDigest(e.Digest) && e.Ref != Ref(prefix, e.Digest) {
				refs[e.Ref] = Ref(prefix, e.Digest)
			}
		}
	}
	for from, to := range refs {
		if from == to {
			delete(refs, from)
		}
	}
	if len(refs) > 0 {
		if err := g.Connections.RepointImages(ctx, t, refs); err != nil {
			return err
		}
	}
	if g.Index != nil {
		if _, err := g.Index.RepointGateway(clusterID, func(e kubeimage.Entry) string { return Ref(prefix, e.Digest) }); err != nil {
			return err
		}
	}
	flog.Info("gateway image references re-pointed", "cluster", clusterID, "from", old, "to", prefix, "references", len(refs))
	return nil
}
