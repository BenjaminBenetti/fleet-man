package kubeimage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
)

type fakeGateway struct {
	ok      bool
	why     string
	removed []string
}

func (g *fakeGateway) Available(Target) (bool, string) { return g.ok, g.why }

func (g *fakeGateway) Deliver(_ context.Context, _ Target, img Image) (GatewayDelivery, error) {
	return GatewayDelivery{Ref: "gw.example.com/rid/devcontainer@" + img.Config.ID, Digest: img.Config.ID, Config: fakeGatewayConfig(img.Config.ID), PullSecret: "fleet-gateway"}, nil
}

// fakeGatewayConfig is the config digest the fake gateway's manifest of the
// image imageID names.
func fakeGatewayConfig(imageID string) string { return sha256Digest([]byte("config of " + imageID)) }

func (g *fakeGateway) Remove(_ context.Context, _ Target, e Entry) error {
	g.removed = append(g.removed, e.Ref)
	return nil
}

func TestResolve(t *testing.T) {
	// Only the fake tools on the connection's extra PATH exist (CI runners
	// have real kind and minikube on PATH).
	t.Setenv("PATH", t.TempDir())
	h := newHarness(t)
	minikube := localcluster.Cluster{Kind: localcluster.Minikube, Name: "dev"}
	kindNoTool := localcluster.Cluster{Kind: localcluster.KinD, Name: "x"}
	withReg := func(c *fleet.KubeCluster) { c.Image.Registry = "registry.example.com/team" }
	mode := func(m fleet.KubeImageMode) func(*fleet.KubeCluster) {
		return func(c *fleet.KubeCluster) { c.Image.Mode = m }
	}
	noExtraPath := func(c *fleet.KubeCluster) { c.ExtraPath = nil }
	gwOn := &fakeGateway{ok: true}
	gwOff := &fakeGateway{why: "Enable Image Registry is off"}

	tests := []struct {
		name    string
		cluster *fleet.KubeCluster
		local   localcluster.Cluster
		gw      Gateway
		want    Strategy
		reason  string
	}{
		{"auto local", h.cluster(), minikube, nil, StrategyNodeLoad, "local cluster (minikube:dev)"},
		{"auto local wins over a registry", h.cluster(withReg), minikube, gwOn, StrategyNodeLoad, ""},
		{"auto registry", h.cluster(withReg), localcluster.Cluster{}, gwOn, StrategyRegistry, "registry set (registry.example.com/team)"},
		{"auto local without its tool falls to the registry", h.cluster(withReg, noExtraPath), kindNoTool, nil, StrategyRegistry, ""},
		{"auto gateway", h.cluster(), localcluster.Cluster{}, gwOn, StrategyGateway, "the gateway serves images"},
		{"auto nothing", h.cluster(), localcluster.Cluster{}, gwOff, StrategyNone, "Enable Image Registry is off"},
		{"auto nothing without a gateway", h.cluster(), localcluster.Cluster{}, nil, StrategyNone, "gateway delivery is not available"},
		{"auto nothing names the missing tool", h.cluster(noExtraPath), kindNoTool, nil, StrategyNone, "needs kind"},
		{"auto bad registry", h.cluster(func(c *fleet.KubeCluster) { c.Image.Registry = "myuser" }), localcluster.Cluster{}, nil, StrategyNone, "must start with a registry host"},
		{"node-load on a remote cluster", h.cluster(mode(fleet.KubeImageNodeLoad)), localcluster.Cluster{}, gwOn, StrategyNone, "needs a local cluster"},
		{"node-load", h.cluster(mode(fleet.KubeImageNodeLoad)), minikube, nil, StrategyNodeLoad, ""},
		{"registry without one", h.cluster(mode(fleet.KubeImageRegistry)), minikube, gwOn, StrategyNone, "no registry is set"},
		{"registry", h.cluster(mode(fleet.KubeImageRegistry), withReg), minikube, nil, StrategyRegistry, ""},
		{"gateway off", h.cluster(mode(fleet.KubeImageGateway)), minikube, gwOff, StrategyNone, "Enable Image Registry is off"},
		{"gateway", h.cluster(mode(fleet.KubeImageGateway), withReg), minikube, gwOn, StrategyGateway, ""},
		{"unknown mode", h.cluster(mode("teleport")), minikube, nil, StrategyNone, "unknown image delivery mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.m.Gateway = tt.gw
			got := h.m.Resolve(h.target(tt.cluster, tt.local))
			if got.Strategy != tt.want || !strings.Contains(got.Reason, tt.reason) {
				t.Errorf("Resolve = %+v, want %s (%q)", got, tt.want, tt.reason)
			}
			if got.Strategy == StrategyNone && tt.cluster.Image.Mode == "" && !strings.Contains(got.Reason, "Settings") {
				t.Errorf("no Settings hint: %q", got.Reason)
			}
		})
	}
	h.m.Gateway = nil
	if got := h.m.Resolve(Target{Cluster: h.cluster(), DaemonID: "short"}); got.Strategy != StrategyNone {
		t.Errorf("a short daemon id resolves to %+v", got)
	}
}

func TestRetention(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 72 * time.Hour, "24h": 24 * time.Hour, "0": 0, "90m": 90 * time.Minute, "nonsense": 72 * time.Hour, "-1h": 72 * time.Hour} {
		c := &fleet.KubeCluster{Image: fleet.KubeImageSettings{Retention: in}}
		if got := Retention(c); got != want {
			t.Errorf("Retention(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBuiltBy(t *testing.T) {
	call := func(args ...string) kubeshim.Call {
		return kubeshim.Call{Binary: "docker", Args: args, Outcome: kubeshim.OutcomePass}
	}
	imageOnly := []kubeshim.Call{call("-v"), call("buildx", "version"), call("inspect", "--type", "image", "ubuntu"), call("pull", "ubuntu"), call("events")}
	if BuiltBy(imageOnly) {
		t.Error("an image-only capture counts as built")
	}
	for _, c := range []kubeshim.Call{call("buildx", "build", "-t", "x", "."), call("build", "-t", "x", "."), call("image", "build", "."), call("--context", "default", "buildx", "build", ".")} {
		if !BuiltBy(append(append([]kubeshim.Call(nil), imageOnly...), c)) {
			t.Errorf("%v does not count as a build", c.Args)
		}
	}
	img := NewImage("ubuntu", imageConfig(imageID("u")), imageOnly)
	if img.Built || img.Ref != "ubuntu" {
		t.Errorf("NewImage %+v", img)
	}
}

func TestValidateSettings(t *testing.T) {
	pace := func(p int) *int { return &p }
	ok := []fleet.KubeImageSettings{
		{},
		{Mode: fleet.KubeImageRegistry, Registry: "registry.example.com/team", Retention: "24h", PullSecrets: []string{"team-pull", "a.b"}},
		{Mode: fleet.KubeImageNodeLoad, Retention: "0", UploadPacePercent: pace(0)},
		{Mode: fleet.KubeImageGateway, UploadPacePercent: pace(100)},
	}
	for _, s := range ok {
		if err := ValidateSettings(s); err != nil {
			t.Errorf("ValidateSettings(%+v): %v", s, err)
		}
	}
	bad := []fleet.KubeImageSettings{
		{Mode: "teleport"},
		{Registry: "myuser"},
		{Retention: "3 days"},
		{Retention: "-1h"},
		{PullSecrets: []string{"Bad_Name"}},
		{UploadPacePercent: pace(101)},
	}
	for _, s := range bad {
		if err := ValidateSettings(s); err == nil {
			t.Errorf("ValidateSettings(%+v) passed", s)
		}
	}
}
