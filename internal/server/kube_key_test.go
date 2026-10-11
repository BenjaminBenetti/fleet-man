package server

import (
	"context"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/owner"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

const keyTestDaemonID = "0123456789abcdef0123456789abcdef"

func stubDaemonIdentity(t *testing.T) {
	t.Helper()
	orig := loadDaemonIdentity
	loadDaemonIdentity = func() (owner.Identity, error) { return owner.Identity{DaemonID: keyTestDaemonID}, nil }
	t.Cleanup(func() { loadDaemonIdentity = orig })
}

// An accepted Kubernetes create's CREATING record carries the instance's
// key (of the daemon, the fleet, the instance and its creation time) before
// its job runs, which applies nothing without it.
func TestCreateInstanceGivesAKubernetesRecordItsKey(t *testing.T) {
	isolateFleetDir(t)
	stubDaemonIdentity(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Remote: "git@x:a.git"}}}); err != nil {
		t.Fatal(err)
	}
	if err := state.UpdateKubeClusters(func(k *state.KubeClusters) error {
		k.Clusters = []fleet.KubeCluster{{ID: "minikube-abc234", Context: "minikube", Namespace: "fleet"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seen := make(chan *fleet.Instance, 1)
	orig := jobRunCreate
	jobRunCreate = func(_ context.Context, fleetName, instName, _, _ string, _ bool, bt fleet.BackendType) error {
		st, err := state.Load()
		if err != nil {
			return err
		}
		inst, err := st.Fleets[fleetName].GetInstance(instName)
		if err != nil {
			return err
		}
		seen <- inst
		return nil
	}
	t.Cleanup(func() { jobRunCreate = orig })
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	if err := createErr(t, client, &fleetgrpc.CreateInstanceRequest{
		Fleet: "alpha", Instance: "k1", Backend: fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES, KubeCluster: "minikube-abc234",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	var inst *fleet.Instance
	select {
	case inst = <-seen:
	case <-time.After(10 * time.Second):
		t.Fatal("the create job never ran")
	}
	if inst.Backend != fleet.BackendKubernetes || inst.KubeCluster != "minikube-abc234" || inst.Status != fleet.StatusCreating {
		t.Fatalf("record %+v", inst)
	}
	if want := names.InstanceKey(keyTestDaemonID, "alpha", "k1", inst.CreatedAt); inst.KubeKey != want {
		t.Fatalf("key %q, want %q (of its creation time %v)", inst.KubeKey, want, inst.CreatedAt)
	}
}

// A key another record holds moves the creation time on until one is free.
func TestAssignKubeKeyAvoidsATakenKey(t *testing.T) {
	stubDaemonIdentity(t)
	created := time.Unix(1700000000, 0).UTC()
	taken := names.InstanceKey(keyTestDaemonID, "alpha", "k1", created)
	st := &state.State{Fleets: map[string]*fleet.Fleet{"beta": {Name: "beta", Instances: []*fleet.Instance{{Name: "x", KubeKey: taken}}}}}
	inst := &fleet.Instance{Name: "k1", CreatedAt: created}
	if err := assignKubeKey(st, "alpha", inst); err != nil {
		t.Fatal(err)
	}
	if inst.KubeKey == taken || !names.ValidHash(inst.KubeKey) || !inst.CreatedAt.After(created) ||
		inst.KubeKey != names.InstanceKey(keyTestDaemonID, "alpha", "k1", inst.CreatedAt) {
		t.Fatalf("key %q at %v (taken %q at %v)", inst.KubeKey, inst.CreatedAt, taken, created)
	}
	free := &fleet.Instance{Name: "k2", CreatedAt: created}
	if err := assignKubeKey(st, "alpha", free); err != nil || !free.CreatedAt.Equal(created) {
		t.Fatalf("a free key moved the creation time: %v %v", free.CreatedAt, err)
	}
}
