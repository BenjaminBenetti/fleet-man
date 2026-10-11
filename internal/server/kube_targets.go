//go:build !nok8s

package server

import (
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// deployTargets lists one Deploy option per connection, in registry order.
// A cluster target is available when the connection is READY or LIMITED and
// the daemon host has docker and the devcontainer CLI (images are built
// there); otherwise the reason says which is missing. A template (file://)
// fleet lists them too: its workspace is copied on the daemon host, and the
// seed takes it into the pod from there, as it does a clone.
func (m *kubeManager) deployTargets() []fleet.DeployTargetOption {
	var missingTools []string
	for _, tool := range deps.HostBuildTools(daemonLookPath) {
		if !tool.Found {
			missingTools = append(missingTools, tool.Binary)
		}
	}
	var out []fleet.DeployTargetOption
	for _, st := range m.statuses() {
		option := fleet.DeployTargetOption{Target: fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: st.GetCluster().GetId()}, Available: true}
		switch {
		case !preflight.Usable(st.GetState()):
			option.Available, option.Reason = false, clusterStateReason(st)
		case len(missingTools) > 0:
			option.Available, option.Reason = false, strings.Join(missingTools, " and ")+" not found where fleetd runs"
		}
		out = append(out, option)
	}
	return out
}

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

// createAllowed checks a Kubernetes create against its connection before
// the CREATING record is written: the connection must exist, and its
// quotas (max_instances, max_total_disk) must have room for one more
// instance. st is the state the record will be added to. Other targets pass.
func (m *kubeManager) createAllowed(st *state.State, target fleet.DeployTarget) error {
	if target.Backend != fleet.BackendKubernetes {
		return nil
	}
	if target.KubeCluster == "" {
		return status.Error(codes.InvalidArgument, "name the Kubernetes connection to create the instance on (there is no remembered or only one)")
	}
	rec, ok := m.record(target.KubeCluster)
	if !ok {
		return status.Errorf(codes.NotFound, "no Kubernetes connection %q", target.KubeCluster)
	}
	used := 0
	for _, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.KubeCluster == rec.ID && inst.Status != fleet.StatusDeleting {
				used++
			}
		}
	}
	if rec.MaxInstances > 0 && used >= rec.MaxInstances {
		return status.Errorf(codes.ResourceExhausted, "kubernetes:quota: connection %q allows %d instances and has %d", rec.GetDisplayName(), rec.MaxInstances, used)
	}
	if rec.MaxTotalDisk != "" {
		max, err := resource.ParseQuantity(rec.MaxTotalDisk)
		disk, derr := resource.ParseQuantity(orDefaultString(rec.DiskSize, fleet.DefaultKubeDiskSize))
		if err == nil && derr == nil {
			total := disk.DeepCopy()
			for i := 0; i < used; i++ {
				total.Add(disk)
			}
			if total.Cmp(max) > 0 {
				return status.Errorf(codes.ResourceExhausted, "kubernetes:quota: connection %q allows %s of instance disks; %d more of %s would take %s", rec.GetDisplayName(), max.String(), used+1, disk.String(), total.String())
			}
		}
	}
	return nil
}

func orDefaultString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
