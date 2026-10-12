//go:build !nok8s

package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	kubebackend "github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/imageserve"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/server/remote"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_image.go is how the images fleetd builds reach its Kubernetes
// connections: one kubeimage.Manager for the daemon (node load, registry,
// gateway) and, while the gateway tunnel can come up (the local MCP server
// runs), the gateway image server (internal/k8s/imageserve) on the tunnel's
// registry listener ("Enable Image Registry"). Each connection's full
// preflight judges its delivery through it (the image_delivery check, and
// the deep test's pull through the gateway), its pushed status carries the
// gateway's live numbers (last pull, cache size, upload pace), and removing
// a connection lets go of everything delivered to it.

// kubeImages is the daemon's image delivery.
type kubeImages struct {
	manager *kubeimage.Manager
	// gateway is the gateway image server, nil without a tunnel to serve
	// it on; uplink paces its uploads.
	gateway *imageserve.Gateway
	uplink  *imageserve.Uplink

	sizeMu sync.Mutex
	size   int64
	sizeAt time.Time
}

// noGatewayReason is why a daemon without a registry listener offers no
// Gateway delivery.
const noGatewayReason = "gateway delivery needs the local MCP server, which carries the gateway tunnel (it is not running)"

// gatewayCacheSizeTTL paces reading the gateway cache's size.
const gatewayCacheSizeTTL = 10 * time.Second

// Delays before the connections react to the gateway: a pull's statistics
// are pushed at most this often, and a registry that came or went is
// re-judged (a full preflight of the connections it can deliver to) once
// it has settled.
var (
	kubePullPushDelay    = time.Second
	kubeRegistrySettle   = 5 * time.Second
	kubeImageCleanupTime = 2 * time.Minute
)

// startImages gives the connections image delivery. With withGateway (the
// tunnel can come up) it also serves the gateway image server on a
// registry listener until ctx ends, and returns the tunnel options that
// wire it; attachTunnel then hands it the tunnel.
func (m *kubeManager) startImages(ctx context.Context, withGateway bool) []remote.Option {
	im := &kubeImages{manager: &kubeimage.Manager{IndexPath: state.KubeImagesPath}}
	var opts []remote.Option
	if withGateway {
		im.uplink = &imageserve.Uplink{}
		im.gateway = &imageserve.Gateway{
			Store:    &imageserve.Store{Dir: state.KubeImageCacheDir()},
			Clusters: &imageserve.StateClusters{},
			Uplink:   im.uplink,
			Index:    im.manager,
			OnPull:   m.pullProgress,
			// A registry id or host change re-points the gateway-delivered
			// instances at once.
			Connections: kubebackend.ImageConnections{},
		}
		im.manager.Gateway = im.gateway
		lis := remote.NewChanListener()
		srv := &http.Server{Handler: im.gateway.Handler(), ReadHeaderTimeout: time.Minute}
		go func() { _ = srv.Serve(lis) }()
		context.AfterFunc(ctx, func() { _ = srv.Close() })
		opts = []remote.Option{
			remote.WithRegistryListener(lis),
			remote.WithRegistryChange(im.gateway.RegistryChanged),
			remote.WithRegistryState(m.registryStateChanged),
			remote.WithRegistryConnWrapper(im.uplink.Meter),
		}
	}
	m.mu.Lock()
	m.images = im
	m.mu.Unlock()
	m.setBackendEnvironment(im)
	return opts
}

// attachTunnel hands the gateway image server the tunnel it serves
// through.
func (m *kubeManager) attachTunnel(mgr *remote.Manager) {
	if im := m.imagesOrNil(); im != nil && im.gateway != nil {
		im.gateway.Tunnel = mgr
	}
}

func (m *kubeManager) imagesOrNil() *kubeImages {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.images
}

// pullProgress pushes the connections' statuses shortly after a pull
// through the gateway moved (Gateway.OnPull), at most once per delay.
func (m *kubeManager) pullProgress(string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pullPush != nil {
		return
	}
	m.pullPush = time.AfterFunc(kubePullPushDelay, func() {
		m.mu.Lock()
		m.pullPush = nil
		m.mu.Unlock()
		m.publish()
	})
}

// registryStateChanged is told the tunnel's image registry state
// (remote.WithRegistryState). The statuses show Gateway delivery's
// availability live at once; once the state has settled, every connection
// whose delivery the gateway decides and whose last full preflight began
// before the change runs another, so its resolved delivery and manifest
// check follow.
func (m *kubeManager) registryStateChanged(string, string) {
	go m.publish()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registryChangedAt = m.now()
	if m.registrySettle != nil {
		m.registrySettle.Reset(kubeRegistrySettle)
		return
	}
	m.registrySettle = time.AfterFunc(kubeRegistrySettle, func() {
		m.mu.Lock()
		m.registrySettle = nil
		changed := m.registryChangedAt
		workers := make([]*kubeWorker, 0, len(m.workers))
		for _, w := range m.workers {
			workers = append(workers, w)
		}
		m.mu.Unlock()
		for _, w := range workers {
			w.mu.Lock()
			gw := w.haveFull && !w.fullStarted.After(changed) && usesGateway(w.rec, w.full.Capabilities.GetImageDelivery().GetMode())
			if gw {
				w.fullWanted = true
			}
			w.mu.Unlock()
			if gw {
				w.poke()
			}
		}
	})
}

// usesGateway reports whether the gateway decides a connection's delivery,
// given what its last full preflight resolved: set to Gateway, or Auto, when
// it resolved to the gateway or to nothing. Node load and a set registry
// come first in Auto's order (a local cluster whose tool is missing on the
// daemon host falls through to the gateway too); a connection not judged yet
// has its first full preflight to come.
func usesGateway(rec fleet.KubeCluster, resolved string) bool {
	switch rec.Image.Mode {
	case fleet.KubeImageGateway, "", fleet.KubeImageAuto:
		return resolved == string(kubeimage.StrategyGateway) || resolved == string(kubeimage.StrategyNone)
	}
	return false
}

// delivery is the preflight's view of the daemon's image delivery for one
// connection; nil without image delivery or a daemon identity.
func (m *kubeManager) delivery(rec fleet.KubeCluster, link *kubeLink, saved bool) preflight.Delivery {
	im := m.imagesOrNil()
	if im == nil {
		return nil
	}
	id, err := m.identity()
	if err != nil {
		return nil
	}
	return &kubeDelivery{im: im, rec: rec, link: link, daemonID: id.DaemonID, owner: id.Owner(), saved: saved}
}

// kubeDelivery is preflight.Delivery for one connection.
type kubeDelivery struct {
	im       *kubeImages
	rec      fleet.KubeCluster
	link     *kubeLink
	daemonID string
	owner    string
	// saved: rec is a stored connection (a draft has no id, credential or
	// index).
	saved bool
}

// draftClusterID stands in for an unsaved connection's id while its
// delivery is resolved (nothing is written under it).
const draftClusterID = "draft"

// pullSecretLabels are the labels of a connection's managed pull Secrets.
func pullSecretLabels(owner, clusterID string) map[string]string {
	return kubebackend.PullSecretLabels(owner, clusterID)
}

func (d *kubeDelivery) target(local localcluster.Cluster, platform string) kubeimage.Target {
	rec := d.rec
	if !d.saved {
		rec.ID = draftClusterID
	}
	return kubeimage.Target{Cluster: &rec, DaemonID: d.daemonID, Local: local, Platform: platform, Labels: pullSecretLabels(d.owner, rec.ID)}
}

// Check implements preflight.Delivery.
func (d *kubeDelivery) Check(ctx context.Context, local localcluster.Cluster, platform string) preflight.DeliveryCheck {
	t := d.target(local, platform)
	var st kubeimage.DeliveryStatus
	if d.saved {
		st = d.im.manager.Status(ctx, t)
	} else {
		// A draft's registry check would write an index under no
		// connection: it waits for the save.
		res := d.im.manager.Resolve(t)
		st = kubeimage.DeliveryStatus{Mode: string(res.Strategy), Reason: res.Reason}
	}
	del := &fleetgrpc.KubeImageDelivery{
		Mode: st.Mode, Reason: st.Reason,
		RegistryReachable: st.RegistryReachable, RegistryDeleteSupported: st.RegistryDeleteSupported,
	}
	d.im.gatewayFacts(del)
	pass := func(detail string) preflight.DeliveryCheck {
		return preflight.DeliveryCheck{Delivery: del, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_PASS, Detail: detail}
	}
	switch kubeimage.Strategy(st.Mode) {
	case kubeimage.StrategyNodeLoad:
		return pass("node load: " + st.Reason)
	case kubeimage.StrategyRegistry:
		reg := strings.TrimSpace(d.rec.Image.Registry)
		switch {
		case !d.saved:
			return pass("registry " + reg + " (reachability is checked once the connection is saved)")
		case !st.RegistryReachable:
			return preflight.DeliveryCheck{Delivery: del, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL,
				Detail: "registry: " + st.Reason, Fix: "check the registry address and the daemon host's login to it (docker login)",
				Limited: "image registry unreachable"}
		case !st.RegistryDeleteSupported:
			return preflight.DeliveryCheck{Delivery: del, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_WARN,
				Detail: "registry " + reg + " reachable; " + st.Reason}
		}
		return pass("registry " + reg + " reachable, deletes images past retention")
	case kubeimage.StrategyGateway:
		rc := d.im.checkGateway(ctx, t, d.saved)
		del.GatewayManifestOk, del.GatewayFailedStep = rc.Step == "", rc.Step
		if rc.Step != "" {
			return preflight.DeliveryCheck{Delivery: del, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL,
				Detail: "gateway " + rc.Step + ": " + rc.Message, Fix: rc.Hint,
				Limited: "gateway delivery failing (" + rc.Step + ")"}
		}
		if !d.saved {
			return pass("gateway: route ok (" + rc.Host + "); the credential is checked once the connection is saved")
		}
		return pass("gateway: route + credential ok (" + rc.Host + ")")
	}
	return preflight.DeliveryCheck{Delivery: del, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL,
		Detail: st.Reason,
		Fix:    "set a registry in the cluster's image settings, or turn on Enable Image Registry (Settings → Fleet MCP) for Gateway delivery",
		// Image-only devcontainers still run (their image passes through
		// from its registry); a build cannot reach the cluster.
		Limited: "built images cannot be delivered"}
}

// checkGateway fetches the probe image's manifest through the gateway's
// public URL: with the connection's credential once it is saved, else
// route and TLS only.
func (im *kubeImages) checkGateway(ctx context.Context, t kubeimage.Target, saved bool) imageserve.RouteCheck {
	if im.gateway == nil {
		return imageserve.RouteCheck{Step: imageserve.StepRoute, Message: noGatewayReason}
	}
	if !saved {
		digest, err := imageserve.ProbeDigest(t.Platform)
		if err != nil {
			return imageserve.RouteCheck{Step: imageserve.StepDigest, Message: err.Error()}
		}
		return im.gateway.CheckRoute(ctx, "", digest)
	}
	probe, err := im.gateway.Probe(ctx, t)
	if err != nil {
		return imageserve.RouteCheck{Step: imageserve.StepDigest, Message: "the probe image could not be served: " + err.Error()}
	}
	return im.gateway.CheckRoute(ctx, t.Cluster.ID, probe.Digest)
}

// PullProbe implements preflight.Delivery: the probe image served to the
// connection, its gateway pull Secret applied in the namespace.
func (d *kubeDelivery) PullProbe(ctx context.Context, platform string) (string, string, error) {
	if d.im.gateway == nil {
		return "", "", errors.New(noGatewayReason)
	}
	if !d.saved || d.link == nil {
		return "", "", errors.New("save the connection to test a pull through the gateway")
	}
	t := d.target(localcluster.Cluster{}, platform)
	t.Secrets = kubebackend.PullSecrets{Client: d.link.client, Namespace: d.rec.Namespace}
	p, err := d.im.gateway.Probe(ctx, t)
	if err != nil {
		return "", "", err
	}
	return p.Ref, p.PullSecret, nil
}

// ClassifyPull implements preflight.Delivery.
func (d *kubeDelivery) ClassifyPull(ref, reason, message string) (string, string, string) {
	if d.im.gateway == nil {
		return "", "pulling " + ref + " failed: " + message, ""
	}
	pf, _ := d.im.gateway.ClassifyPull(d.rec.ID, ref, reason, message)
	return imageserve.PullStep(pf.Kind), pf.Message, pf.Hint
}

// gatewayFacts fills what a delivery says of the gateway right now: whether
// it serves images for this daemon, why not, and its host.
func (im *kubeImages) gatewayFacts(del *fleetgrpc.KubeImageDelivery) {
	del.GatewayRegistry, del.GatewayReason, del.GatewayHost = false, noGatewayReason, ""
	if im.gateway == nil || im.gateway.Tunnel == nil {
		return
	}
	prefix, why := im.gateway.Tunnel.Registry()
	del.GatewayRegistry, del.GatewayReason = prefix != "", why
	if prefix != "" {
		del.GatewayReason = ""
		del.GatewayHost, _, _ = strings.Cut(prefix, "/")
	}
}

// live fills a connection's delivery with what the gateway shows now: its
// availability, the connection's last pull through it, the cache's size and
// the upload pace.
func (im *kubeImages) live(del *fleetgrpc.KubeImageDelivery, rec fleet.KubeCluster) {
	im.gatewayFacts(del)
	del.UploadPacePercent = int32(imageserve.PacePercent(rec.Image))
	if im.gateway == nil {
		return
	}
	if p, ok := im.gateway.Handler().LastPull(rec.ID); ok {
		pull := &fleetgrpc.KubeImagePull{Manifest: p.Manifest, StartedAt: timestamppb.New(p.At), Bytes: p.Bytes}
		if !p.Last.IsZero() {
			pull.LastByteAt = timestamppb.New(p.Last)
		}
		del.LastPull = pull
	}
	del.CacheBytes = im.cacheSize()
	if im.uplink != nil {
		del.UplinkBytesPerSecond = int64(im.uplink.Rate())
	}
}

func (im *kubeImages) cacheSize() int64 {
	im.sizeMu.Lock()
	defer im.sizeMu.Unlock()
	if !im.sizeAt.IsZero() && time.Since(im.sizeAt) < gatewayCacheSizeTTL {
		return im.size
	}
	if n, err := im.gateway.Store.Size(); err == nil {
		im.size = n
	}
	im.sizeAt = time.Now()
	return im.size
}

// leaseHeld reports whether this daemon holds a connection's owner Lease
// (its running worker's).
func (m *kubeManager) leaseHeld(id string) bool {
	w := m.worker(id)
	if w == nil {
		return false
	}
	w.mu.Lock()
	l := w.lease
	w.mu.Unlock()
	return l != nil && l.Holding()
}

// releaseImages lets go of everything delivered to a removed connection:
// the gateway cache stops serving it at once; then, in the background and
// bounded, every image delivered to it is collected (kubeimage's GC, as if
// no instance held one), its managed pull Secrets are deleted (only while
// this daemon held the connection's owner Lease: a restored copy of
// ~/.fleet must not delete the Secrets the original still pulls with), and
// its directory under ~/.fleet/kube goes, unless a new connection took the
// id meanwhile.
func (m *kubeManager) releaseImages(rec fleet.KubeCluster, leaseHeld bool) {
	im := m.imagesOrNil()
	id, idErr := m.identity()
	if im == nil || idErr != nil {
		return
	}
	t := kubeimage.Target{Cluster: &rec, DaemonID: id.DaemonID, Labels: pullSecretLabels(id.Owner(), rec.ID)}
	if im.gateway != nil {
		if err := im.gateway.Forget(context.Background(), t); err != nil {
			flog.Warn("kube: forget the removed connection's gateway images", "cluster", rec.ID, "err", err)
		}
	}
	m.cleanup(func(ctx context.Context) {
		var seedCaches func(context.Context) ([]string, error)
		if leaseHeld {
			if link, err := m.connect(rec, m.environ()); err == nil {
				t.Secrets = kubebackend.PullSecrets{Client: link.client, Namespace: rec.Namespace}
				seedCaches = func(ctx context.Context) ([]string, error) {
					return kubebackend.DeleteSeedCaches(ctx, link.client, rec.Namespace, id.Owner(), rec.ID)
				}
			}
		}
		if _, err := os.Stat(state.KubeImagesPath(rec.ID)); err == nil {
			res, err := im.manager.GC(ctx, t, kubeimage.GCOptions{All: true, Live: func(string) bool { return false }})
			if err != nil {
				flog.Warn("kube: collect the removed connection's images", "cluster", rec.ID, "err", err)
			} else if len(res.Kept) > 0 || len(res.Warnings) > 0 {
				flog.Warn("kube: some of the removed connection's images stay", "cluster", rec.ID, "kept", len(res.Kept), "warnings", strings.Join(res.Warnings, "; "))
			}
		}
		if seedCaches != nil {
			if deleted, err := seedCaches(ctx); err != nil {
				flog.Warn("kube: delete the removed connection's seed caches", "cluster", rec.ID, "err", preflight.Message(err))
			} else if len(deleted) > 0 {
				flog.Info("kube: deleted the removed connection's seed caches", "cluster", rec.ID, "deleted", strings.Join(deleted, ", "))
			}
		}
		if t.Secrets != nil {
			for _, name := range []string{kubeimage.PullSecretName(id.DaemonID, rec.ID), imageserve.GatewayPullSecretName(id.DaemonID, rec.ID)} {
				if err := t.Secrets.DeletePullSecret(ctx, name); err != nil {
					flog.Warn("kube: delete the removed connection's pull Secret", "cluster", rec.ID, "secret", name, "err", preflight.Message(err))
				}
			}
		}
		if _, still := m.record(rec.ID); !still {
			if err := os.RemoveAll(state.KubeClusterDir(rec.ID)); err != nil {
				flog.Warn("kube: remove the connection's directory", "cluster", rec.ID, "err", err)
			}
		}
	})
}

// cleanup runs work in the background, bounded by kubeImageCleanupTime and
// ended by the daemon's shutdown (the serve loop's context), as one of the
// collections waitCleanups waits for.
func (m *kubeManager) cleanup(work func(ctx context.Context)) {
	m.mu.Lock()
	base := m.ctx
	m.mu.Unlock()
	if base == nil {
		base = context.Background()
	}
	m.cleanups.add()
	go func() {
		defer m.cleanups.done()
		ctx, cancel := context.WithTimeout(base, kubeImageCleanupTime)
		defer cancel()
		work(ctx)
	}()
}

// waitCleanups waits for the background collections until ctx ends, and
// reports whether they all ended.
func (m *kubeManager) waitCleanups(ctx context.Context) bool {
	return m.cleanups.wait(ctx)
}

// cleanupSet counts the collections running in the background. Unlike a
// sync.WaitGroup it takes a new one while someone waits (a removal that an
// RPC still in the shutdown's drain makes, as the shutdown waits), and a
// wait that gives up leaves nothing behind.
type cleanupSet struct {
	mu      sync.Mutex
	running int
	// idle is closed once running drops to 0 (nil while none ran).
	idle chan struct{}
}

func (c *cleanupSet) add() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running == 0 {
		c.idle = make(chan struct{})
	}
	c.running++
}

func (c *cleanupSet) done() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running--; c.running == 0 {
		close(c.idle)
	}
}

// wait reports whether every collection ended (including ones started while
// it waited) before ctx did.
func (c *cleanupSet) wait(ctx context.Context) bool {
	for {
		c.mu.Lock()
		running, idle := c.running, c.idle
		c.mu.Unlock()
		if running == 0 {
			return true
		}
		select {
		case <-idle:
		case <-ctx.Done():
			return false
		}
	}
}
