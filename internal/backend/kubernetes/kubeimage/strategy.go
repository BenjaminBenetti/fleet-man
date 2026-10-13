package kubeimage

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

// Strategy is how one image reaches a cluster.
type Strategy string

const (
	// StrategyPassthrough: an image the CLI did not build, pulled from its
	// own registry by digest (repo@sha256).
	StrategyPassthrough Strategy = "passthrough"
	// StrategyNodeLoad: loaded into a local cluster's nodes from the daemon
	// host's docker, pulled Never.
	StrategyNodeLoad Strategy = Strategy(fleet.KubeImageNodeLoad)
	// StrategyRegistry: pushed to the cluster's registry under the daemon's
	// own path (<registry>/fleet/<daemon8>/devcontainer:<id12>) and pulled
	// by digest.
	StrategyRegistry Strategy = Strategy(fleet.KubeImageRegistry)
	// StrategyGateway: served by fleetd through the Fleet gateway.
	StrategyGateway Strategy = Strategy(fleet.KubeImageGateway)
	// StrategyNone: nothing can deliver a built image to the cluster.
	StrategyNone Strategy = "none"
)

// Pull policies a Delivery asks for.
const (
	PullNever        = "Never"
	PullIfNotPresent = "IfNotPresent"
)

// Error codes of delivery failures (the kubernetes:<code> error prefixes).
const (
	// CodeNoDelivery: a built image and no registry, gateway or local
	// cluster to deliver it by.
	CodeNoDelivery = "kubernetes:unsupported:image_delivery"
	// CodeImagePush: pushing or loading the image failed.
	CodeImagePush = "kubernetes:image_push"
	// CodeImagePull: a pod cannot get its image, and delivering it again
	// cannot help.
	CodeImagePull = "kubernetes:image_pull"
)

// Error is a delivery failure: an error code, the cause and, when there is
// a known one, the fix.
type Error struct {
	Code string
	Err  error
	Hint string
}

func (e *Error) Error() string {
	s := e.Code + ": " + e.Err.Error()
	if e.Hint != "" {
		s += "; " + e.Hint
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// Target is the cluster connection an image is delivered to.
type Target struct {
	// Cluster is the connection's record (its id and image settings).
	Cluster *fleet.KubeCluster
	// DaemonID is the daemon's id (~/.fleet/daemon_id); the registry path
	// and the managed Secret's name carry it.
	DaemonID string
	// Local is the local cluster the connection's nodes belong to
	// (localcluster.Detect); zero for any other cluster.
	Local localcluster.Cluster
	// Platform is the nodes' platform ("linux/amd64").
	Platform string
	// Secrets manages the connection's managed pull Secret in its
	// namespace; nil when the caller has none (no Secret is then managed).
	Secrets Secrets
	// Labels go on the objects delivery creates in the namespace: the
	// connection's base labels (names.Base), never an instance's key label,
	// since the managed pull Secret outlives every instance.
	Labels map[string]string
}

func (t Target) validate() error {
	if t.Cluster == nil {
		return fmt.Errorf("image delivery: no cluster")
	}
	if err := validateClusterID(t.Cluster.ID); err != nil {
		return err
	}
	return validateDaemonID(t.DaemonID)
}

// validateClusterID refuses a cluster id that is not one path segment (it
// names the index's directory).
func validateClusterID(id string) error {
	if id == "" || strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		return fmt.Errorf("image delivery: invalid cluster id %q", id)
	}
	return nil
}

// registry is the connection's configured registry; ok is false when none
// is set (or it does not parse).
func (t Target) registry() (Registry, bool, error) {
	if strings.TrimSpace(t.Cluster.Image.Registry) == "" {
		return Registry{}, false, nil
	}
	r, err := ParseRegistry(t.Cluster.Image.Registry)
	if err != nil {
		return Registry{}, false, err
	}
	return r, true, nil
}

// insecure reports whether domain is the connection's registry and set
// insecure.
func (t Target) insecure(domain string) bool {
	r, ok, _ := t.registry()
	return ok && r.Domain == domain && t.Cluster.Image.InsecureRegistry
}

// ValidateSettings checks a connection's image settings as the daemon
// stores them: a known mode, a registry that parses, a retention that is a
// Go duration (or "0"), and an upload pace within 0-100.
func ValidateSettings(s fleet.KubeImageSettings) error {
	if !s.Mode.Valid() {
		return fmt.Errorf("unknown image delivery mode %q (auto, node-load, registry, gateway)", s.Mode)
	}
	if strings.TrimSpace(s.Registry) != "" {
		if _, err := ParseRegistry(s.Registry); err != nil {
			return err
		}
	}
	if r := strings.TrimSpace(s.Retention); r != "" && r != "0" {
		d, err := time.ParseDuration(r)
		if err != nil || d < 0 {
			return fmt.Errorf("image retention %q is not a duration (72h, 30m)", s.Retention)
		}
	}
	for _, name := range s.PullSecrets {
		if !secretName.MatchString(strings.TrimSpace(name)) {
			return fmt.Errorf("pull secret %q is not a Secret name", name)
		}
	}
	if p := s.UploadPacePercent; p != nil && (*p < 0 || *p > 100) {
		return fmt.Errorf("upload pace %d%% is not within 0-100", *p)
	}
	return nil
}

// secretName is a Secret name (an RFC 1123 subdomain).
var secretName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// Retention is how long an image nothing uses any more is kept: the
// connection's image.retention, else DefaultKubeImageRetention. "0" collects
// it at the next pass.
func Retention(c *fleet.KubeCluster) time.Duration {
	s := strings.TrimSpace(c.Image.Retention)
	if s == "" {
		return fleet.DefaultKubeImageRetention
	}
	if s == "0" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return fleet.DefaultKubeImageRetention
	}
	return d
}

// Image is a built (or pulled) image on the daemon host, as capture-up
// found it.
type Image struct {
	// Ref is the image the captured docker run names (CaptureResult.Run.Image).
	Ref string
	// Config is its config on the daemon host (CaptureResult.Image).
	Config kubeshim.ImageConfig
	// Built reports whether the CLI built it during the capture; only an
	// image it did not build can pass through from its registry.
	Built bool
}

// NewImage is the Image of a capture: the run's image, its config and the
// shim's journal of the CLI's docker calls (CaptureResult.Run.Image, .Image
// and .Calls).
func NewImage(ref string, cfg kubeshim.ImageConfig, calls []kubeshim.Call) Image {
	return Image{Ref: ref, Config: cfg, Built: BuiltBy(calls)}
}

// BuiltBy reports whether the CLI built an image: any build call in the
// shim's journal (a Dockerfile, features, or the CLI's own uid layer).
func BuiltBy(calls []kubeshim.Call) bool {
	for _, c := range calls {
		switch c.Verb() {
		case "build", "buildx build", "image build", "builder build":
			return true
		}
	}
	return false
}

// Gateway serves images through the Fleet gateway (fleetd's image server,
// internal/k8s/imageserve). Its implementation lands with the image server;
// a Manager without one has no gateway delivery.
type Gateway interface {
	// Available reports whether the gateway can serve this connection's
	// images now, and why not.
	Available(t Target) (bool, string)
	// Deliver makes img servable to the connection's cluster.
	Deliver(ctx context.Context, t Target, img Image) (GatewayDelivery, error)
	// Remove stops serving an image the GC collects.
	Remove(ctx context.Context, t Target, e Entry) error
}

// GatewayDelivery is where the gateway serves an image.
type GatewayDelivery struct {
	// Ref is "<public-host>/<rid>/devcontainer@sha256:<digest>".
	Ref string
	// Digest is the platform manifest's digest, Config that manifest's
	// config digest.
	Digest, Config string
	// Location names what serves it, for the index.
	Location string
	// PullSecret is the namespace's gateway pull Secret.
	PullSecret string
}

// Resolution is the strategy a connection's built images take.
type Resolution struct {
	Strategy Strategy
	// Reason says why Auto picked the strategy, or, for StrategyNone (or an
	// explicit mode that cannot work), why nothing can deliver.
	Reason string
}

// noDeliveryHint is what to change when nothing can deliver.
const noDeliveryHint = "set a registry in the cluster's image settings (Settings → Kubernetes), or turn on Enable Image Registry for gateway delivery"

// Resolve picks how the connection's built images are delivered. Auto takes
// node load on a local cluster (minikube, kind, k3d, Docker Desktop), then
// the registry when one is set, then the gateway when it serves images, else
// nothing; an explicit mode is taken as is, failing with the reason it
// cannot work.
func (m *Manager) Resolve(t Target) Resolution {
	if err := t.validate(); err != nil {
		return Resolution{Strategy: StrategyNone, Reason: err.Error()}
	}
	reg, hasReg, regErr := t.registry()
	gwOK, gwWhy := false, "gateway delivery is not available in this daemon"
	if m.Gateway != nil {
		gwOK, gwWhy = m.Gateway.Available(t)
	}
	switch mode := t.Cluster.Image.Mode; mode {
	case fleet.KubeImageNodeLoad:
		if _, err := m.loader(t.Local, t); err != nil {
			return Resolution{Strategy: StrategyNone, Reason: err.Error()}
		}
		return Resolution{Strategy: StrategyNodeLoad, Reason: "delivery is set to node load"}
	case fleet.KubeImageRegistry:
		switch {
		case regErr != nil:
			return Resolution{Strategy: StrategyNone, Reason: regErr.Error()}
		case !hasReg:
			return Resolution{Strategy: StrategyNone, Reason: "delivery is set to registry, but no registry is set"}
		}
		return Resolution{Strategy: StrategyRegistry, Reason: "delivery is set to registry (" + reg.String() + ")"}
	case fleet.KubeImageGateway:
		if !gwOK {
			return Resolution{Strategy: StrategyNone, Reason: "delivery is set to gateway, but " + gwWhy}
		}
		return Resolution{Strategy: StrategyGateway, Reason: "delivery is set to gateway"}
	case "", fleet.KubeImageAuto:
	default:
		return Resolution{Strategy: StrategyNone, Reason: fmt.Sprintf("unknown image delivery mode %q", mode)}
	}

	var notes []string
	if t.Local.Local() {
		_, err := m.loader(t.Local, t)
		if err == nil {
			return Resolution{Strategy: StrategyNodeLoad, Reason: "local cluster (" + t.Local.String() + ")"}
		}
		notes = append(notes, err.Error())
	}
	if regErr != nil {
		notes = append(notes, regErr.Error())
	} else if hasReg {
		return Resolution{Strategy: StrategyRegistry, Reason: "registry set (" + reg.String() + ")"}
	}
	if gwOK {
		return Resolution{Strategy: StrategyGateway, Reason: "no registry set; the gateway serves images"}
	}
	notes = append(notes, "no registry is set", gwWhy)
	return Resolution{Strategy: StrategyNone, Reason: "a built image cannot reach this cluster: " + strings.Join(notes, "; ") + "; " + noDeliveryHint}
}
