package server

import (
	"context"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// kube_target_test.go covers what the daemon does with Kubernetes deploy
// targets: it carries the cluster wherever a target travels, refuses a
// create before any record exists while it runs without
// FLEET_EXPERIMENTAL_K8S, and gives an accepted create's record its key.

// Without FLEET_EXPERIMENTAL_K8S, a create that names Kubernetes is refused
// up front (no FAILED record left behind, nothing remembered), from the
// local socket as from any client.
func TestCreateInstanceRefusesKubernetesBeforeAnyRecord(t *testing.T) {
	isolateFleetDir(t)
	t.Setenv(fleet.EnvExperimentalK8s, "")
	ran := false
	orig := jobRunCreate
	jobRunCreate = func(context.Context, string, string, string, string, bool, fleet.BackendType) error {
		ran = true
		return nil
	}
	t.Cleanup(func() { jobRunCreate = orig })
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git"},
	}}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	err := createErr(t, client, &fleetgrpc.CreateInstanceRequest{
		Fleet: "alpha", Instance: "k1", Backend: fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES,
		KubeCluster: "minikube", RememberDefault: true,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Kubernetes create error = %v, want InvalidArgument", err)
	}
	if ran {
		t.Fatal("a refused create still ran its provisioning job")
	}
	st, err := state.Load()
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	if _, err := st.Fleets["alpha"].GetInstance("k1"); err == nil {
		t.Fatal("a refused Kubernetes create left a record behind")
	}
	if got := storedDefaultBackend(t); got != "" {
		t.Fatalf("a refused create remembered default %q", got)
	}
}

// The remembered default carries its cluster, and SetConfig (any client's
// whole-config save) can't change either half of it.
func TestRememberedDefaultKeepsItsCluster(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	target := fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "prod-eu"}
	if err := svc.rememberDefaultTarget(target); err != nil {
		t.Fatalf("rememberDefaultTarget: %v", err)
	}
	cfg, err := state.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultBackend != string(fleet.BackendKubernetes) || cfg.DefaultKubeCluster != "prod-eu" {
		t.Fatalf("remembered %q/%q, want kubernetes/prod-eu", cfg.DefaultBackend, cfg.DefaultKubeCluster)
	}

	if _, err := svc.SetConfig(context.Background(), &fleetgrpc.SetConfigRequest{Config: &fleetgrpc.Config{
		Agent:              &fleetgrpc.AgentSettings{ToolSelection: "claude"},
		DefaultBackend:     fleetgrpc.BackendType_BACKEND_TYPE_CODER,
		DefaultKubeCluster: "stale-copy",
	}}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if cfg, err = state.LoadConfig(); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultBackend != string(fleet.BackendKubernetes) || cfg.DefaultKubeCluster != "prod-eu" {
		t.Fatalf("SetConfig changed the remembered default to %q/%q", cfg.DefaultBackend, cfg.DefaultKubeCluster)
	}

	// Remembering a target without a cluster clears the old one.
	if err := svc.rememberDefaultTarget(fleet.DeployTarget{Backend: fleet.BackendCoder}); err != nil {
		t.Fatalf("rememberDefaultTarget: %v", err)
	}
	if cfg, err = state.LoadConfig(); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultBackend != string(fleet.BackendCoder) || cfg.DefaultKubeCluster != "" {
		t.Fatalf("remembered %q/%q, want coder and no cluster", cfg.DefaultBackend, cfg.DefaultKubeCluster)
	}
}

// A remembered Kubernetes default is not one a create can use yet, so an
// unspecified create still gets devcontainer.
func TestRememberedKubernetesDefaultFallsBackToDevcontainer(t *testing.T) {
	isolateFleetDir(t)
	stubDaemonHost(t, "h", "devcontainer")
	if err := state.SaveConfig(&state.Config{DefaultBackend: string(fleet.BackendKubernetes), DefaultKubeCluster: "minikube"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	def := listTargets(t, newService(), "").GetDefaultTarget()
	if def.GetBackend() != fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER || def.GetKubeCluster() != "" {
		t.Fatalf("default target = %v, want devcontainer", def)
	}
}

// The cluster travels with a request only for a Kubernetes target.
func TestResolveTargetCarriesTheClusterOnlyForKubernetes(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	tests := []struct {
		req  *fleetgrpc.CreateInstanceRequest
		want fleet.DeployTarget
	}{
		{&fleetgrpc.CreateInstanceRequest{Backend: fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES, KubeCluster: "minikube"},
			fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "minikube"}},
		{&fleetgrpc.CreateInstanceRequest{Backend: fleetgrpc.BackendType_BACKEND_TYPE_CODER, KubeCluster: "minikube"},
			fleet.DeployTarget{Backend: fleet.BackendCoder}},
		{&fleetgrpc.CreateInstanceRequest{KubeCluster: "minikube"},
			fleet.DeployTarget{Backend: fleet.BackendDevcontainer}},
	}
	for _, tt := range tests {
		if got := svc.resolveTarget(tt.req, "git@x:a.git"); got != tt.want {
			t.Errorf("resolveTarget(%v) = %+v, want %+v", tt.req, got, tt.want)
		}
	}
}

// A clone runs on its source's cluster connection, with a key of its own.
func TestCloneDestinationInheritsTheSourceCluster(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{
			Name: "src", Status: fleet.StatusRunning, ContainerID: "c1",
			Backend: fleet.BackendKubernetes, KubeCluster: "prod-eu", KubeKey: "srckey12",
		}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var dest fleet.Instance
	orig := jobRunClone
	jobRunClone = func(_ context.Context, f, _, destName string, _ bool) error {
		st, err := state.Load()
		if err != nil {
			return err
		}
		inst, err := st.Fleets[f].GetInstance(destName)
		if err != nil {
			return err
		}
		dest = *inst
		return nil
	}
	t.Cleanup(func() { jobRunClone = orig })
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.CloneInstance(context.Background(), &fleetgrpc.CloneInstanceRequest{Fleet: "alpha", SourceInstance: "src", NewInstance: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	drainJob(t, stream)
	if dest.Backend != fleet.BackendKubernetes || dest.KubeCluster != "prod-eu" {
		t.Fatalf("clone destination ran on %q/%q, want kubernetes/prod-eu", dest.Backend, dest.KubeCluster)
	}
	if dest.KubeKey != "" {
		t.Fatalf("clone destination took the source's key %q", dest.KubeKey)
	}
}
