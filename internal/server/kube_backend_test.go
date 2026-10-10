//go:build !nok8s

package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/localcluster"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
)

// backendRig is a manager with one connection's worker, its last full
// preflight set by the test.
func backendRig(t *testing.T) (*kubeManager, *kubeWorker) {
	t.Helper()
	m := newKubeManager(nil)
	w := &kubeWorker{m: m, wake: make(chan struct{}, 1), rec: fleet.KubeCluster{ID: "mk-abc234", DisplayName: "laptop", Context: "minikube"}}
	m.workers = map[string]*kubeWorker{"mk-abc234": w}
	return m, w
}

// A create gets the connection's facts from its last full preflight.
func TestBackendFactsFromTheLastFullPreflight(t *testing.T) {
	m, w := backendRig(t)
	caps := &fleetgrpc.KubeCapabilities{LocalCluster: true, Admission: &fleetgrpc.KubeAdmission{Unprivileged: true}}
	w.haveFull, w.full = true, preflight.Result{State: fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_LIMITED, Capabilities: caps,
		Local: localcluster.Cluster{Kind: localcluster.Minikube, Name: "minikube"}, Platform: "linux/amd64"}
	f, err := m.backendFacts(context.Background(), "mk-abc234")
	if err != nil || f.Capabilities != caps || f.Platform != "linux/amd64" || f.Local.Kind != localcluster.Minikube {
		t.Fatalf("facts %+v, %v", f, err)
	}
	if _, err := m.backendFacts(context.Background(), "gone-abc234"); !hasCode(err, kubeerr.ClusterMissing) {
		t.Errorf("a connection that is gone: %v", err)
	}
}

// An unusable connection refuses the create with its state's code.
func TestBackendFactsOfAnUnusableConnection(t *testing.T) {
	m, w := backendRig(t)
	for state, code := range map[fleetgrpc.KubeClusterState]kubeerr.Code{
		fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNREACHABLE:     kubeerr.Unreachable,
		fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED:    kubeerr.Unauthorized,
		fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_FORBIDDEN:       kubeerr.Forbidden,
		fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING: kubeerr.Misconfigured,
		fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_PLUGIN_MISSING:  kubeerr.Misconfigured,
		fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED:   kubeerr.Misconfigured,
		fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_OWNER_CONFLICT:  kubeerr.OwnerConflict,
	} {
		w.mu.Lock()
		w.haveFull, w.full = true, preflight.Result{State: state, Message: "the cluster says no"}
		w.mu.Unlock()
		_, err := m.backendFacts(context.Background(), "mk-abc234")
		if !hasCode(err, code) || !strings.Contains(err.Error(), "laptop") || !strings.Contains(err.Error(), "the cluster says no") {
			t.Errorf("%s: %v, want %s naming the connection and why", state, err, code)
		}
	}
}

// A connection not checked yet is asked for a full preflight and waited for.
func TestBackendFactsWaitForTheFirstPreflight(t *testing.T) {
	m, w := backendRig(t)
	origWait, origPoll := kubeFactsWait, kubeFactsPoll
	kubeFactsWait, kubeFactsPoll = 2*time.Second, 10*time.Millisecond
	t.Cleanup(func() { kubeFactsWait, kubeFactsPoll = origWait, origPoll })
	go func() {
		<-w.wake
		w.mu.Lock()
		wanted := w.fullWanted
		w.haveFull, w.full = true, preflight.Result{State: fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, Capabilities: &fleetgrpc.KubeCapabilities{}}
		w.mu.Unlock()
		if !wanted {
			t.Error("the wait did not ask for a full preflight")
		}
	}()
	if _, err := m.backendFacts(context.Background(), "mk-abc234"); err != nil {
		t.Fatal(err)
	}

	// None comes: the create fails, naming the wait.
	m2, _ := backendRig(t)
	kubeFactsWait = 50 * time.Millisecond
	if _, err := m2.backendFacts(context.Background(), "mk-abc234"); !hasCode(err, kubeerr.Unreachable) || !strings.Contains(err.Error(), "not been checked yet") {
		t.Errorf("no preflight: %v", err)
	}
}

func hasCode(err error, code kubeerr.Code) bool {
	c, ok := kubeerr.Of(err)
	return ok && c == code
}
