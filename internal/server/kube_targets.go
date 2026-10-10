//go:build !nok8s

package server

import (
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
)

// deployTargets lists one Deploy option per connection, in registry order,
// while FLEET_EXPERIMENTAL_K8S is on (the backend is still being built). A
// cluster target is available when the connection is READY or LIMITED and
// the daemon host has docker and the devcontainer CLI (images are built
// there); otherwise the reason says which is missing. Template (file://)
// fleets list none: copying a template into a pod is not supported yet.
func (m *kubeManager) deployTargets(template bool) []fleet.DeployTargetOption {
	if !fleet.ExperimentalK8s() || template {
		return nil
	}
	var missingTools []string
	for _, tool := range deps.HostBuildTools(daemonLookPath) {
		if !tool.Found {
			missingTools = append(missingTools, tool.Binary)
		}
	}
	creatable := kubeBackendCreatable()
	var out []fleet.DeployTargetOption
	for _, st := range m.statuses() {
		option := fleet.DeployTargetOption{Target: fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: st.GetCluster().GetId()}, Available: true}
		switch {
		case !creatable:
			option.Available, option.Reason = false, "this fleet build cannot create Kubernetes instances yet"
		case !preflight.Usable(st.GetState()):
			option.Available, option.Reason = false, clusterStateReason(st)
		case len(missingTools) > 0:
			option.Available, option.Reason = false, strings.Join(missingTools, " and ")+" not found where fleetd runs"
		}
		out = append(out, option)
	}
	return out
}

// kubeBackendCreatable reports whether this build creates Kubernetes
// instances; a seam for tests until the backend lands.
var kubeBackendCreatable = func() bool { return fleet.ValidateBackendType(fleet.BackendKubernetes) == nil }

// clusterStateReason says why a connection in st cannot take a create.
func clusterStateReason(st *fleetgrpc.KubeClusterStatus) string {
	label := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(st.GetState().String(), "KUBE_CLUSTER_STATE_"), "_", " "))
	if st.GetState() == fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CHECKING {
		return "checking the cluster"
	}
	reason := "cluster " + label
	if msg := st.GetMessage(); msg != "" {
		reason += ": " + msg
	}
	return reason
}

// defaultCluster is the connection a Kubernetes create that names none goes
// to: the remembered one when it still exists, else the only connection;
// "" when that does not decide.
func (m *kubeManager) defaultCluster(remembered string) string {
	statuses := m.statuses()
	for _, st := range statuses {
		if remembered != "" && st.GetCluster().GetId() == remembered {
			return remembered
		}
	}
	if len(statuses) == 1 {
		return statuses[0].GetCluster().GetId()
	}
	return ""
}
