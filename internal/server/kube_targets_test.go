//go:build !nok8s

package server

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	kubecheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// clusterStatus is a connection's status as the probes leave it.
func clusterStatus(name string, state fleetgrpc.KubeClusterState, caps *fleetgrpc.KubeCapabilities) *fleetgrpc.KubeClusterStatus {
	return &fleetgrpc.KubeClusterStatus{Cluster: &fleetgrpc.KubeCluster{Id: name + "-id", DisplayName: name}, State: state, Capabilities: caps}
}

// The findings of a fleet's devcontainer.json decide a cluster target with
// its health: compose makes it unavailable whatever the health; a warning
// rides an available target only.
func TestClusterTargetOption(t *testing.T) {
	ready := fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY
	closed := &fleetgrpc.KubeCapabilities{Version: "v1.33.0", Admission: &fleetgrpc.KubeAdmission{Unprivileged: true}}
	dind := kubecheck.Config{Privileged: "Docker-in-Docker"}
	for _, tc := range []struct {
		name    string
		st      *fleetgrpc.KubeClusterStatus
		missing []string
		repo    kubecheck.Config
		want    fleet.DeployTargetOption
	}{
		{"compose on a ready cluster", clusterStatus("c", ready, closed), nil, kubecheck.Config{Compose: true},
			fleet.DeployTargetOption{Reason: kubecheck.ComposeRefusal}},
		{"compose wins over the cluster's health", clusterStatus("c", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE, nil), nil, kubecheck.Config{Compose: true},
			fleet.DeployTargetOption{Reason: kubecheck.ComposeRefusal}},
		{"dind on a cluster without privileged pods", clusterStatus("c", ready, closed), nil, dind,
			fleet.DeployTargetOption{Available: true, Warnings: []string{"c: Docker-in-Docker needs privileged pods, which are not allowed there (Allow privileged pods, Settings → Kubernetes)"}}},
		{"no warning on an unavailable target", clusterStatus("c", ready, closed), []string{"docker"}, dind,
			fleet.DeployTargetOption{Reason: "docker not found where fleetd runs"}},
		{"nothing to say", clusterStatus("c", ready, closed), nil, kubecheck.Config{},
			fleet.DeployTargetOption{Available: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.want.Target = fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "c-id"}
			if got := clusterTargetOption(tc.st, tc.missing, tc.repo, true); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("clusterTargetOption = %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// checkCluster reads the probes' findings the way a create's translation
// does.
func TestCheckCluster(t *testing.T) {
	sysboxCaps := &fleetgrpc.KubeCapabilities{RuntimeClasses: []string{"gvisor", "sysbox-runc"}, Admission: &fleetgrpc.KubeAdmission{Unprivileged: true}}
	for _, tc := range []struct {
		name string
		st   *fleetgrpc.KubeClusterStatus
		want kubecheck.Cluster
	}{
		{"not checked yet", &fleetgrpc.KubeClusterStatus{Cluster: &fleetgrpc.KubeCluster{Id: "x"}}, kubecheck.Cluster{Name: "x"}},
		{"root admitted, privileged allowed", clusterStatus("a", 0, &fleetgrpc.KubeCapabilities{Version: "v1.30.0", PrivilegedAllowed: true, Admission: &fleetgrpc.KubeAdmission{Unprivileged: true}}),
			kubecheck.Cluster{Name: "a", Known: true, PrivilegedAllowed: true, Version: "v1.30.0"}},
		{"no root", clusterStatus("b", 0, &fleetgrpc.KubeCapabilities{Admission: &fleetgrpc.KubeAdmission{}}),
			kubecheck.Cluster{Name: "b", Known: true, NoRoot: true}},
		{"admission not read", clusterStatus("c", 0, &fleetgrpc.KubeCapabilities{}),
			kubecheck.Cluster{Name: "c", Known: true}},
		{"a sysbox class on the cluster", clusterStatus("d", 0, sysboxCaps),
			kubecheck.Cluster{Name: "d", Known: true, Sysbox: true}},
		{"the connection pins another runtime", &fleetgrpc.KubeClusterStatus{Cluster: &fleetgrpc.KubeCluster{DisplayName: "e", RuntimeClass: "gvisor"}, Capabilities: sysboxCaps},
			kubecheck.Cluster{Name: "e", Known: true}},
		{"the connection pins sysbox", &fleetgrpc.KubeClusterStatus{Cluster: &fleetgrpc.KubeCluster{DisplayName: "f", RuntimeClass: "sysbox-runc"}, Capabilities: &fleetgrpc.KubeCapabilities{}},
			kubecheck.Cluster{Name: "f", Known: true, Sysbox: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkCluster(tc.st); got != tc.want {
				t.Errorf("checkCluster = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Over the wire: a compose fleet's cluster targets are unavailable with the
// reason, while its other targets are offered as before.
func TestListDeployTargetsOfAComposeFleet(t *testing.T) {
	f := newKubeFixture(t)
	stubDaemonHost(t, "h", "devcontainer", "docker")
	f.start()
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", DisplayName: "mk"}).GetCluster().GetId()
	f.awaitState(id, fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY)

	ws := t.TempDir()
	writeDevcontainer(t, ws, `{"dockerComposeFile": "docker-compose.yml", "service": "app"}`)
	st := &state.State{Fleets: map[string]*fleet.Fleet{"c": {Name: "c", Remote: "git@example.com:c.git",
		Instances: []*fleet.Instance{{Name: "one", WorkspaceDir: ws, Status: fleet.StatusRunning}}}}}
	if err := state.Save(st); err != nil {
		t.Fatal(err)
	}
	reply, err := f.svc.ListDeployTargets(context.Background(), &fleetgrpc.ListDeployTargetsRequest{Fleet: "c"})
	if err != nil {
		t.Fatal(err)
	}
	var cluster *fleetgrpc.DeployTargetOption
	devcontainerOffered := false
	for _, o := range reply.GetTargets() {
		switch o.GetTarget().GetBackend() {
		case fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES:
			cluster = o
		case fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER:
			devcontainerOffered = o.GetAvailable()
		}
	}
	if cluster == nil || cluster.GetAvailable() || cluster.GetReason() != kubecheck.ComposeRefusal {
		t.Errorf("the cluster target of a compose fleet = %v", cluster)
	}
	if !devcontainerOffered {
		t.Error("a compose fleet lost its devcontainer target")
	}
	// Another fleet, or none, is not judged by c's configuration.
	reply, err = f.svc.ListDeployTargets(context.Background(), &fleetgrpc.ListDeployTargetsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range reply.GetTargets() {
		if o.GetTarget().GetBackend() == fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES && (!o.GetAvailable() || strings.Contains(o.GetReason(), "compose")) {
			t.Errorf("no fleet named: %v", o)
		}
	}
}
