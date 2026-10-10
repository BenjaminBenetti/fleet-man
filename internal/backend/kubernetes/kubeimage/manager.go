package kubeimage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
)

// Manager delivers built images to clusters and keeps each connection's
// image index. One Manager serves every connection of the daemon; its
// methods are safe for concurrent use.
type Manager struct {
	// Docker is the daemon host's docker: a path, or a name looked up on
	// PATH; "" is "docker".
	Docker string
	// IndexPath is where a connection's image index lives
	// (state.KubeImagesPath). Each connection's index sits in a directory of
	// its own under one parent: before the GC deletes what connections can
	// share (a registry manifest, a local cluster's copy, a host tag), it
	// reads the other indexes there.
	IndexPath func(clusterID string) string
	// Credentials finds the daemon host's registry logins; nil reads
	// docker's config.
	Credentials Credentials
	// Gateway is gateway delivery; nil leaves it unavailable.
	Gateway Gateway
	// Transport overrides the registry client's HTTP transport (tests).
	Transport http.RoundTripper
	// Now overrides the clock (tests).
	Now func() time.Time
	// passthroughTimeout bounds the registry check of a passthrough
	// candidate (tests shorten it).
	passthroughTimeout time.Duration

	files      indexFiles
	keys       keyLocks
	deliveries flight[Entry]
}

// defaultPassthroughTimeout bounds asking a registry whether it holds an
// image the CLI did not build.
const defaultPassthroughTimeout = 20 * time.Second

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) credentials() Credentials {
	if m.Credentials == nil {
		return DockerCredentials{}
	}
	return m.Credentials
}

func (m *Manager) docker() (dockerCLI, error) {
	path := m.Docker
	if path == "" {
		path = "docker"
	}
	if !strings.Contains(path, "/") {
		found, err := exec.LookPath(path)
		if err != nil {
			return dockerCLI{}, fmt.Errorf("docker is not installed on the daemon host: %w", err)
		}
		path = found
	}
	abs, err := filepath.Abs(path)
	return dockerCLI{path: abs}, err
}

func (m *Manager) indexPath(t Target) (string, error) {
	if err := m.checkIndex(t.Cluster.ID); err != nil {
		return "", err
	}
	return m.IndexPath(t.Cluster.ID), nil
}

// checkIndex refuses a Manager without an index and a cluster id that is
// not one path segment.
func (m *Manager) checkIndex(clusterID string) error {
	if m.IndexPath == nil {
		return errors.New("image delivery: no image index path")
	}
	return validateClusterID(clusterID)
}

func (m *Manager) loader(c localcluster.Cluster, t Target) (nodeLoader, error) {
	docker, err := m.docker()
	if err != nil && c.Kind == localcluster.DockerDesktop {
		return nil, err
	}
	return loaderFor(c, t.Cluster.ExtraPath, docker)
}

// Delivery is how an instance pod gets its image.
type Delivery struct {
	Strategy Strategy
	// Ref is the pod's image: repo@digest, or the loaded tag for node load.
	Ref string
	// PullPolicy is PullNever for node load, PullIfNotPresent otherwise.
	PullPolicy string
	// PullSecrets are the pod's imagePullSecrets: the connection's
	// configured ones, then the managed one.
	PullSecrets []string
	// ImageID is the daemon host's image ID.
	ImageID string
	// Digest is the manifest digest Ref pins; "" for node load.
	Digest string
	// Tag is the tag fleet put on the image on the daemon host.
	Tag string
	// Location is where the image went (Entry.Location).
	Location string
	// Warnings are non-fatal problems, also reported to the job's log.
	Warnings []string
}

func deliveryOf(e Entry) *Delivery {
	d := &Delivery{Strategy: e.Strategy, Ref: e.Ref, PullPolicy: PullIfNotPresent, ImageID: e.ImageID, Digest: e.Digest, Tag: e.Tag, Location: e.Location}
	if e.Strategy == StrategyNodeLoad {
		d.PullPolicy = PullNever
	}
	if e.PullSecret != "" {
		d.PullSecrets = []string{e.PullSecret}
	}
	return d
}

// maxHoldAttempts bounds re-deliveries when the GC collected an image
// between its delivery and its holder taking it.
const maxHoldAttempts = 3

// Deliver makes img available to the target cluster for holder (an
// instance's key, a seed cache) and returns how its pods reference it. An
// image the CLI did not build, which its registry holds, passes through by
// digest; a built one goes by the connection's strategy (Resolve). The image
// is recorded in the connection's index, held by holder, which lets go of
// any other image of the index it held (an instance runs one image; a
// rebuild that must keep the old one until it succeeds holds the new one
// under another holder). Concurrent deliveries of one image share one push
// or load.
//
// Progress goes to ctx's provisioning log.
func (m *Manager) Deliver(ctx context.Context, t Target, img Image, holder string) (*Delivery, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	if holder == "" {
		return nil, errors.New("image delivery: no holder")
	}
	if !digestPattern.MatchString(img.Config.ID) {
		return nil, fmt.Errorf("image delivery: image %q has no image ID", img.Ref)
	}
	path, err := m.indexPath(t)
	if err != nil {
		return nil, err
	}
	log := backend.JobLogFrom(ctx)
	start := m.now()

	var d *Delivery
	if p, ok := m.passthrough(ctx, t, img); ok {
		e := Entry{ImageID: img.Config.ID, Strategy: StrategyPassthrough, Location: p.ref.Repository(), Ref: p.ref.String(), Digest: p.ref.Digest, Platform: t.Platform, DeliveredAt: m.now()}
		if d, err = m.hold(path, e, holder, true); err != nil {
			return nil, err
		}
		if p.private && len(t.Cluster.Image.PullSecrets) == 0 {
			if reg, ok, _ := t.registry(); !ok || reg.Domain != p.ref.Domain {
				d.Warnings = append(d.Warnings, fmt.Sprintf("%s needs a login to pull: add a pull Secret for %s to the cluster's image settings, or set the image registry to %s so fleet manages one", p.ref.Repository(), p.ref.Domain, p.ref.Domain))
			}
		}
		fmt.Fprintf(log, "image %s passes through from its registry: %s\n", id12(img.Config.ID), d.Ref)
	} else {
		res := m.Resolve(t)
		if res.Strategy == StrategyNone {
			return nil, &Error{Code: CodeNoDelivery, Err: errors.New(res.Reason)}
		}
		fmt.Fprintf(log, "delivering image %s to %s by %s (%s)\n", id12(img.Config.ID), t.Cluster.GetDisplayName(), res.Strategy, res.Reason)
		for attempt := 1; ; attempt++ {
			e, err := m.deliverOnce(ctx, t, img, res.Strategy, path)
			if err != nil {
				return nil, err
			}
			if d, err = m.hold(path, e, holder, false); err != nil {
				return nil, err
			}
			if d != nil {
				break
			}
			if attempt == maxHoldAttempts {
				return nil, fmt.Errorf("image delivery: %s was collected %d times while being delivered", e.Ref, attempt)
			}
		}
	}

	domain := ""
	if r, err := ParseReference(d.Ref); err == nil && d.Strategy != StrategyNodeLoad && d.Strategy != StrategyGateway {
		domain = r.Domain
	}
	secrets, warnings, err := m.pullSecretsFor(ctx, t, domain)
	if err != nil {
		return nil, err
	}
	// The connection's Secrets first, then the one the strategy brought.
	d.PullSecrets = append(secrets, d.PullSecrets...)
	d.Warnings = append(d.Warnings, warnings...)
	for _, w := range d.Warnings {
		backend.ReportWarning(ctx, w)
	}
	flog.Info("kubernetes image delivered", "cluster", t.Cluster.ID, "strategy", d.Strategy, "ref", d.Ref, "image_id", d.ImageID, "holder", holder, "ms", m.now().Sub(start).Milliseconds())
	return d, nil
}

// deliverOnce runs one delivery of img by strategy, shared with concurrent
// callers for the same image and serialized against its collection.
func (m *Manager) deliverOnce(ctx context.Context, t Target, img Image, s Strategy, path string) (Entry, error) {
	probe, err := m.entryFor(t, img.Config.ID, s)
	if err != nil {
		return Entry{}, err
	}
	return m.deliveries.do(ctx, path+"|"+probe.key(), func(ctx context.Context) (Entry, error) {
		unlock := m.keys.lock(probe.lockKey())
		defer unlock()
		var cur *Entry
		if ix, err := m.files.read(path); err == nil {
			if c := ix.find(probe.key()); c != nil {
				cur = c
			}
		}
		e, err := m.deliverBy(ctx, t, img, s, probe, cur)
		if err != nil {
			return Entry{}, err
		}
		e.DeliveredAt = m.now()
		e.Platform = t.Platform
		e.GCError = ""
		if err := m.files.update(path, func(ix *Index) error { ix.put(e); return nil }); err != nil {
			return Entry{}, err
		}
		return e, nil
	})
}

// entryFor is the index entry img gets by strategy, before delivery fills
// in the reference.
func (m *Manager) entryFor(t Target, imageID string, s Strategy) (Entry, error) {
	e := Entry{ImageID: imageID, Strategy: s}
	switch s {
	case StrategyNodeLoad:
		e.Location = t.Local.String()
		e.Tag = NodeLoadTag(t.DaemonID, imageID)
	case StrategyRegistry:
		reg, ok, err := t.registry()
		if err != nil || !ok {
			return Entry{}, fmt.Errorf("image delivery: no registry: %v", err)
		}
		e.Location = RegistryRepository(reg, t.DaemonID)
		e.Tag = RegistryTag(reg, t.DaemonID, imageID)
	case StrategyGateway:
		e.Location = "gateway"
	default:
		return Entry{}, fmt.Errorf("image delivery: cannot deliver by %q", s)
	}
	return e, nil
}

// deliverBy does the delivery work of one strategy; cur is the image's
// current index entry, if any.
func (m *Manager) deliverBy(ctx context.Context, t Target, img Image, s Strategy, e Entry, cur *Entry) (Entry, error) {
	log := backend.JobLogFrom(ctx)
	switch s {
	case StrategyNodeLoad:
		return m.nodeLoad(ctx, log, t, img.Config.ID, e, false)
	case StrategyRegistry:
		return m.push(ctx, log, t, img.Config.ID, e, cur)
	case StrategyGateway:
		if m.Gateway == nil {
			return Entry{}, &Error{Code: CodeNoDelivery, Err: errors.New("gateway delivery is not available in this daemon"), Hint: noDeliveryHint}
		}
		g, err := m.Gateway.Deliver(ctx, t, img)
		if err != nil {
			return Entry{}, err
		}
		e.Ref, e.Digest, e.PullSecret = g.Ref, g.Digest, g.PullSecret
		if g.Location != "" {
			e.Location = g.Location
		}
		return e, nil
	}
	return Entry{}, fmt.Errorf("image delivery: cannot deliver by %q", s)
}

// nodeLoad tags the image fleet's way and loads it into the local
// cluster's nodes.
func (m *Manager) nodeLoad(ctx context.Context, log io.Writer, t Target, imageID string, e Entry, force bool) (Entry, error) {
	docker, err := m.docker()
	if err != nil {
		return Entry{}, err
	}
	c, err := parseLocal(e.Location)
	if err != nil {
		return Entry{}, err
	}
	ld, err := m.loader(c, t)
	if err != nil {
		return Entry{}, &Error{Code: CodeImagePush, Err: err}
	}
	if err := docker.tag(ctx, imageID, e.Tag); err != nil {
		return Entry{}, imageGone(err, imageID)
	}
	fmt.Fprintf(log, "loading %s into %s\n", e.Tag, c)
	if err := ld.load(ctx, log, e.Tag, force); err != nil {
		if isContextErr(err) {
			return Entry{}, err
		}
		return Entry{}, &Error{Code: CodeImagePush, Err: fmt.Errorf("loading the image into %s: %w", c, err)}
	}
	e.Ref = e.Tag
	return e, nil
}

// push tags the image under the daemon's registry path and pushes it,
// unless the registry still holds what an earlier push left there.
func (m *Manager) push(ctx context.Context, log io.Writer, t Target, imageID string, e Entry, cur *Entry) (Entry, error) {
	repo, err := ParseReference(e.Location)
	if err != nil {
		return Entry{}, err
	}
	if cur != nil && cur.Digest != "" {
		c, err := m.client(ctx, t, repo.Domain)
		if err == nil {
			if _, err = c.headManifest(ctx, repo.Path, cur.Digest); err == nil {
				fmt.Fprintf(log, "%s is already in the registry\n", cur.Ref)
				return *cur, nil
			}
		}
		if !isNotFound(err) {
			flog.Warn("kubernetes image: could not check the registry for an earlier push; pushing again", "ref", cur.Ref, "err", err)
		}
	}
	docker, err := m.docker()
	if err != nil {
		return Entry{}, err
	}
	if err := docker.tag(ctx, imageID, e.Tag); err != nil {
		return Entry{}, imageGone(err, imageID)
	}
	fmt.Fprintf(log, "pushing %s\n", e.Tag)
	digest, err := docker.push(ctx, log, e.Tag)
	if err != nil {
		if isContextErr(err) {
			return Entry{}, err
		}
		return Entry{}, &Error{Code: CodeImagePush, Err: err, Hint: pushHint(err, repo.Domain)}
	}
	e.Digest = digest
	e.Ref = e.Location + "@" + digest
	return e, nil
}

// imageGone turns a failed tag of the daemon host's image into the reason
// delivery cannot go on.
func imageGone(err error, imageID string) error {
	if isContextErr(err) {
		return err
	}
	if isNoSuchImage(err) {
		return &Error{Code: CodeImagePull, Err: fmt.Errorf("image %s is no longer on the daemon host", id12(imageID)), Hint: "rebuild the instance to build it again"}
	}
	return &Error{Code: CodeImagePush, Err: err}
}

// client is a registry client for domain with the daemon host's login.
func (m *Manager) client(ctx context.Context, t Target, domain string) (*registryClient, error) {
	return RemoteOptions{Credentials: m.credentials(), Insecure: t.insecure(domain), Transport: m.Transport}.client(ctx, domain)
}

// hold makes holder hold the entry with entry's key (first recording entry,
// when record is set) and lets go of any other entry it held. It returns nil
// when the entry is gone (collected).
func (m *Manager) hold(path string, entry Entry, holder string, record bool) (*Delivery, error) {
	unlock := m.keys.lock(entry.lockKey())
	defer unlock()
	key := entry.key()
	var d *Delivery
	err := m.files.update(path, func(ix *Index) error {
		if record {
			ix.put(entry)
		}
		e := ix.find(key)
		if e == nil {
			return nil
		}
		now := m.now()
		for i := range ix.Images {
			if ix.Images[i].key() != key {
				ix.Images[i].release(holder, now)
			}
		}
		e.hold(holder)
		d = deliveryOf(*e)
		return nil
	})
	return d, err
}

// passthroughRef is an image that can pass through from its registry.
type passthroughRef struct {
	ref Reference
	// private: the registry needs the daemon host's login to serve it.
	private bool
}

// passthrough reports whether img can be pulled from its own registry: the
// CLI did not build it, the daemon host has a digest of it for the
// repository the run names, and the registry holds that digest (anonymously,
// or with the daemon host's login). A local image only looks like a registry
// image on docker's containerd store, which records a digest for every
// tag, so the registry has the last word.
func (m *Manager) passthrough(ctx context.Context, t Target, img Image) (passthroughRef, bool) {
	if img.Built {
		return passthroughRef{}, false
	}
	r, err := ParseReference(img.Ref)
	if err != nil {
		return passthroughRef{}, false
	}
	if r.Digest == "" {
		for _, rd := range img.Config.RepoDigests {
			if p, err := ParseReference(rd); err == nil && p.Repository() == r.Repository() && p.Digest != "" {
				r.Digest = p.Digest
				break
			}
		}
	}
	if r.Digest == "" {
		return passthroughRef{}, false
	}
	r.Tag = ""
	timeout := m.passthroughTimeout
	if timeout == 0 {
		timeout = defaultPassthroughTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	anon := newRegistryClient(r.Domain, t.insecure(r.Domain), Credential{}, m.Transport)
	_, err = anon.headManifest(ctx, r.Path, r.Digest)
	if err == nil {
		return passthroughRef{ref: r}, true
	}
	if !isDenied(err) && !isNotFound(err) {
		flog.Info("kubernetes image: registry check of an unbuilt image failed; delivering it as built", "ref", r.String(), "err", err)
		return passthroughRef{}, false
	}
	authed, cerr := m.client(ctx, t, r.Domain)
	if cerr != nil || authed.cred.Empty() {
		return passthroughRef{}, false
	}
	if _, err := authed.headManifest(ctx, r.Path, r.Digest); err != nil {
		return passthroughRef{}, false
	}
	return passthroughRef{ref: r, private: true}, true
}

// parseLocal reads a node load entry's location back into its cluster.
func parseLocal(location string) (localcluster.Cluster, error) {
	kind, name, ok := strings.Cut(location, ":")
	c := localcluster.Cluster{Kind: localcluster.Kind(kind), Name: name}
	if !ok || name == "" || !slices.Contains([]localcluster.Kind{localcluster.Minikube, localcluster.KinD, localcluster.K3d, localcluster.DockerDesktop}, c.Kind) {
		return localcluster.Cluster{}, fmt.Errorf("image delivery: %q is not a local cluster", location)
	}
	return c, nil
}

// Release lets holder go of every image of the connection, starting their
// retention clocks (an instance was destroyed).
func (m *Manager) Release(clusterID, holder string) error {
	if err := m.checkIndex(clusterID); err != nil {
		return err
	}
	now := m.now()
	return m.files.update(m.IndexPath(clusterID), func(ix *Index) error {
		for i := range ix.Images {
			ix.Images[i].release(holder, now)
		}
		return nil
	})
}

// Lookup returns the delivery of the image holder holds on the connection
// (the latest delivered when it holds several), or nil: what Start needs to
// re-deliver it. Its PullSecrets name only a Secret the strategy brought
// (the gateway's); the connection's own come from its record.
func (m *Manager) Lookup(clusterID, holder string) (*Delivery, error) {
	if err := m.checkIndex(clusterID); err != nil {
		return nil, err
	}
	ix, err := m.files.read(m.IndexPath(clusterID))
	if err != nil {
		return nil, err
	}
	var best *Entry
	for i := range ix.Images {
		e := &ix.Images[i]
		if slices.Contains(e.Holders, holder) && (best == nil || e.DeliveredAt.After(best.DeliveredAt)) {
			best = e
		}
	}
	if best == nil {
		return nil, nil
	}
	return deliveryOf(*best), nil
}

// Entries returns a copy of the connection's image index.
func (m *Manager) Entries(clusterID string) ([]Entry, error) {
	if err := m.checkIndex(clusterID); err != nil {
		return nil, err
	}
	ix, err := m.files.read(m.IndexPath(clusterID))
	if err != nil {
		return nil, err
	}
	return ix.Images, nil
}

// NeverPullHint explains an ErrImageNeverPull that a re-delivery did not
// fix.
const NeverPullHint = "the node has the image, but its kubelet will not run it without a pull: it once pulled this image ID under another name and checks pull credentials; use registry delivery for this cluster, or set the kubelet's imagePullCredentialsVerificationPolicy to NeverVerify"

// waiting reasons and messages a kubelet reports for an image it cannot get.
const (
	reasonNeverPull = "ErrImageNeverPull"
	reasonPull      = "ErrImagePull"
	reasonBackOff   = "ImagePullBackOff"
)

// NeedsRedelivery reports whether a container waiting with reason and
// message can be fixed by delivering its image again: a node-loaded image
// the node no longer has (ErrImageNeverPull: a new node, or the kubelet's
// image GC), or a pushed image the registry no longer has.
//
// A node can also refuse a loaded image it has: a kubelet that checks pull
// credentials (KubeletEnsureSecretPulledImages, on by default in recent
// releases) and once pulled the same image ID under another name (from a
// registry, say) answers ErrImageNeverPull for it too. Re-delivery cannot fix
// that, so callers re-deliver once per start and then fail with
// NeverPullHint.
func NeedsRedelivery(s Strategy, reason, message string) bool {
	switch s {
	case StrategyNodeLoad:
		return reason == reasonNeverPull
	case StrategyRegistry:
		if reason != reasonPull && reason != reasonBackOff {
			return false
		}
		msg := strings.ToLower(message)
		return strings.Contains(msg, "not found") || strings.Contains(msg, "manifest unknown")
	}
	return false
}

// Redeliver delivers a pod's image again after NeedsRedelivery said so:
// node load loads it into the nodes once more, registry delivery pushes it
// again (the digest can change, so the caller re-points the pod when
// the returned Ref differs). It fails when the daemon host no longer has the
// image (a rebuild builds it again) or the strategy cannot deliver again.
func (m *Manager) Redeliver(ctx context.Context, t Target, d Delivery) (*Delivery, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	path, err := m.indexPath(t)
	if err != nil {
		return nil, err
	}
	if d.Strategy != StrategyNodeLoad && d.Strategy != StrategyRegistry {
		return nil, &Error{Code: CodeImagePull, Err: fmt.Errorf("%s images cannot be delivered again by fleet (%s)", d.Strategy, d.Ref)}
	}
	if !digestPattern.MatchString(d.ImageID) {
		return nil, fmt.Errorf("image delivery: no image ID to deliver again")
	}
	e, err := m.entryFor(t, d.ImageID, d.Strategy)
	if err != nil {
		return nil, err
	}
	if d.Location != "" {
		e.Location = d.Location
	}
	if d.Tag != "" {
		e.Tag = d.Tag
	}
	log := backend.JobLogFrom(ctx)
	fmt.Fprintf(log, "delivering image %s to %s again (%s)\n", id12(d.ImageID), t.Cluster.GetDisplayName(), d.Strategy)
	got, err := m.deliveries.do(ctx, path+"|"+e.key(), func(ctx context.Context) (Entry, error) {
		unlock := m.keys.lock(e.lockKey())
		defer unlock()
		var out Entry
		var err error
		if d.Strategy == StrategyNodeLoad {
			out, err = m.nodeLoad(ctx, log, t, d.ImageID, e, true)
		} else {
			out, err = m.push(ctx, log, t, d.ImageID, e, nil)
		}
		if err != nil {
			return Entry{}, err
		}
		out.DeliveredAt = m.now()
		out.Platform = t.Platform
		out.GCError = ""
		return out, m.files.update(path, func(ix *Index) error { ix.put(out); return nil })
	})
	if err != nil {
		return nil, err
	}
	nd := deliveryOf(got)
	nd.PullSecrets = d.PullSecrets
	if nd.Strategy == StrategyRegistry {
		repo, _ := ParseReference(nd.Location)
		secrets, warnings, err := m.pullSecretsFor(ctx, t, repo.Domain)
		if err != nil {
			return nil, err
		}
		nd.PullSecrets, nd.Warnings = secrets, warnings
	}
	flog.Info("kubernetes image delivered again", "cluster", t.Cluster.ID, "strategy", nd.Strategy, "ref", nd.Ref, "image_id", nd.ImageID)
	return nd, nil
}
