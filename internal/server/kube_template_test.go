//go:build !nok8s

package server

import (
	"context"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A template (file://) fleet's create with no backend keeps a remembered
// cluster default (the unspecified-backend narrowing keeps devcontainer or a
// cluster), and one naming the cluster outright is accepted too: the copied
// workspace seeds the pod. A branch is still refused before any record.
func TestTemplateFleetCreatesOnTheRememberedCluster(t *testing.T) {
	isolateFleetDir(t)
	stubDaemonIdentity(t)
	const cluster = "minikube-abc234"
	template := "file://" + t.TempDir()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"scratch": {Name: "scratch", Remote: template}}}); err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Clusters = []fleet.KubeCluster{{ID: cluster, Context: "minikube", Namespace: "fleet"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveConfig(&state.Config{DefaultBackend: string(fleet.BackendKubernetes), DefaultKubeCluster: cluster}); err != nil {
		t.Fatal(err)
	}
	type run struct {
		remote  string
		backend fleet.BackendType
		inst    *fleet.Instance
	}
	seen := make(chan run, 2)
	orig := jobRunCreate
	jobRunCreate = func(_ context.Context, fleetName, instName, remote, _ string, _ bool, bt fleet.BackendType) error {
		st, err := state.Load()
		if err != nil {
			return err
		}
		inst, err := st.Fleets[fleetName].GetInstance(instName)
		if err != nil {
			return err
		}
		seen <- run{remote: remote, backend: bt, inst: inst}
		return nil
	}
	t.Cleanup(func() { jobRunCreate = orig })
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	for _, req := range []*fleetgrpc.CreateInstanceRequest{
		{Fleet: "scratch", Instance: "unspecified"},
		{Fleet: "scratch", Instance: "explicit", Backend: fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES, KubeCluster: cluster},
	} {
		if err := createErr(t, client, req); err != nil {
			t.Fatalf("create %s: %v", req.GetInstance(), err)
		}
		var got run
		select {
		case got = <-seen:
		case <-time.After(10 * time.Second):
			t.Fatalf("the create job of %s never ran", req.GetInstance())
		}
		if got.backend != fleet.BackendKubernetes || got.remote != template {
			t.Errorf("%s: provisioned on %s from %q, want kubernetes from the template", req.GetInstance(), got.backend, got.remote)
		}
		if got.inst.Backend != fleet.BackendKubernetes || got.inst.KubeCluster != cluster || !names.ValidHash(got.inst.KubeKey) {
			t.Errorf("%s: record %s on %q (key %q), want kubernetes on %s with a key", req.GetInstance(), got.inst.Backend, got.inst.KubeCluster, got.inst.KubeKey, cluster)
		}
	}

	branch := "main"
	err := createErr(t, client, &fleetgrpc.CreateInstanceRequest{Fleet: "scratch", Instance: "branched", Branch: &branch})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a branch on a template fleet: error = %v, want InvalidArgument", err)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Fleets["scratch"].GetInstance("branched"); err == nil {
		t.Fatal("a refused template create left a record behind")
	}
}
