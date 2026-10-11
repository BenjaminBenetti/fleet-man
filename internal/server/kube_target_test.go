package server

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/buildflavor"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_target_test.go covers what the daemon does with Kubernetes deploy
// targets: it carries the cluster wherever a target travels, and gives an
// accepted create's record its key (kube_create_refusal_test.go: a create on
// a connection it does not have is refused before any record exists).

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

// A remembered Kubernetes default, cluster and all, is what an unspecified
// create gets, in a template fleet too (its copied workspace seeds the pod);
// a remembered Coder default narrows to devcontainer there.
func TestRememberedKubernetesDefaultIsUsed(t *testing.T) {
	isolateFleetDir(t)
	stubDaemonHost(t, "h", "devcontainer")
	if err := state.SaveConfig(&state.Config{DefaultBackend: string(fleet.BackendKubernetes), DefaultKubeCluster: "minikube"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	def := listTargets(t, newService(), "").GetDefaultTarget()
	if def.GetBackend() != fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES || def.GetKubeCluster() != "minikube" {
		t.Fatalf("default target = %v, want kubernetes on minikube", def)
	}
	if got := defaultTarget("file:///srv/template"); got != (fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "minikube"}) {
		t.Fatalf("a template fleet's default = %+v, want the remembered cluster", got)
	}
	if err := state.SaveConfig(&state.Config{DefaultBackend: string(fleet.BackendCoder)}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if got := defaultTarget("file:///srv/template"); got != (fleet.DeployTarget{Backend: fleet.BackendDevcontainer}) {
		t.Fatalf("a template fleet's default with coder remembered = %+v, want devcontainer", got)
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

// A clone runs on its source's cluster connection, with a key of its own on
// its CLONING record before the clone job runs, under the connection's
// quotas.
func TestCloneDestinationInheritsTheSourceCluster(t *testing.T) {
	isolateFleetDir(t)
	stubDaemonIdentity(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{
			Name: "src", Status: fleet.StatusRunning, ContainerID: "c1",
			Backend: fleet.BackendKubernetes, KubeCluster: "prod-eu", KubeKey: "srckey12",
		}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Clusters = []fleet.KubeCluster{{ID: "prod-eu", Context: "prod", Namespace: "fleet", MaxInstances: 2}}
		return nil
	}); err != nil {
		t.Fatal(err)
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
	if want := names.InstanceKey(keyTestDaemonID, "alpha", "copy", dest.CreatedAt); dest.KubeKey != want || dest.KubeKey == "srckey12" {
		t.Fatalf("clone destination key %q, want its own %q", dest.KubeKey, want)
	}

	// The connection allows two instances: a third clone is refused before
	// any record exists (by a daemon with the Kubernetes backend).
	if buildflavor.Current == buildflavor.Pod {
		return
	}
	stream, err = client.CloneInstance(context.Background(), &fleetgrpc.CloneInstanceRequest{Fleet: "alpha", SourceInstance: "src", NewInstance: "third"})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "kubernetes:quota") {
		t.Fatalf("a clone past the quota: %v", err)
	}
	st, _ := state.Load()
	if _, err := st.Fleets["alpha"].GetInstance("third"); err == nil {
		t.Fatal("the refused clone left a record")
	}
}
