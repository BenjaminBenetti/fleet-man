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
	kubecheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// deployTargets lists one Deploy option per connection, in registry order.
// A cluster target is available when the connection is READY or LIMITED and
// the daemon host has docker and the devcontainer CLI (images are built
// there); otherwise the reason says which is missing. A template (file://)
// fleet lists them too: its workspace is copied on the daemon host, and the
// seed takes it into the pod from there, as it does a clone. repo is what
// the fleet's devcontainer.json asks of a cluster (check/kubernetes): a
// compose devcontainer makes every target unavailable, and what a create
// would be refused for or lack on one is that target's warnings; audio says
// whether either audio direction is on.
func (m *kubeManager) deployTargets(repo kubecheck.Config, audio bool) []fleet.DeployTargetOption {
	var missingTools []string
	for _, tool := range deps.HostBuildTools(daemonLookPath) {
		if !tool.Found {
			missingTools = append(missingTools, tool.Binary)
		}
	}
	var out []fleet.DeployTargetOption
	for _, st := range m.statuses() {
		out = append(out, clusterTargetOption(st, missingTools, repo, audio))
	}
	return out
}

// clusterTargetOption is the Deploy option of the connection st reports on.
func clusterTargetOption(st *fleetgrpc.KubeClusterStatus, missingTools []string, repo kubecheck.Config, audio bool) fleet.DeployTargetOption {
	option := fleet.DeployTargetOption{Target: fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: st.GetCluster().GetId()}, Available: true}
	findings := kubecheck.Check(repo, checkCluster(st), audio)
	switch {
	case findings.Refusal != "":
		// The repository's, whatever the cluster's health: no cluster
		// takes it.
		option.Available, option.Reason = false, findings.Refusal
	case !preflight.Usable(st.GetState()):
		option.Available, option.Reason = false, clusterStateReason(st)
	case len(missingTools) > 0:
		option.Available, option.Reason = false, strings.Join(missingTools, " and ")+" not found where fleetd runs"
	default:
		option.Warnings = findings.Warnings
	}
	return option
}

// checkCluster is what check/kubernetes needs of the connection st reports
// on, from its last checks, read the way a create's translation reads them:
// no root when admission refuses root pods (instanceIdentity), sysbox when
// the connection's runtime class is one, or it pins none and the cluster
// offers one (the privilege policy's sysboxClass).
func checkCluster(st *fleetgrpc.KubeClusterStatus) kubecheck.Cluster {
	rec, caps := st.GetCluster(), st.GetCapabilities()
	c := kubecheck.Cluster{Name: rec.DisplayNameOrID()}
	if caps == nil {
		return c
	}
	c.Known = true
	c.NoRoot = caps.GetAdmission() != nil && !caps.GetAdmission().GetUnprivileged()
	c.PrivilegedAllowed = caps.GetPrivilegedAllowed()
	c.Version = caps.GetVersion()
	if rc := rec.GetRuntimeClass(); rc != "" {
		c.Sysbox = strings.Contains(rc, "sysbox")
	} else {
		for _, class := range caps.GetRuntimeClasses() {
			if strings.Contains(class, "sysbox") {
				c.Sysbox = true
			}
		}
	}
	return c
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

// connected reports whether id is one of the daemon's connections.
func (m *kubeManager) connected(id string) bool {
	_, ok := m.record(id)
	return ok
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
	return m.quotaRoom(st, target.KubeCluster, 0)
}

// quotaRoom checks that connection id's quotas have room for one more
// instance beyond the records of st and extra creates not in st yet (the
// automation runs about to be created): kubernetes:quota
// (RESOURCE_EXHAUSTED) when they do not, NOT_FOUND without the connection.
//
// An instance counts while its record holds an instance key, which is to
// say objects in the cluster: a FAILED automation run whose pod and disk
// were released (releaseFailedRun) holds none, and a record being
// destroyed is on its way out. Seed caches (fi-seedcache-*) are fleet's own
// and do not count: one per image, gone a week after their last use.
func (m *kubeManager) quotaRoom(st *state.State, id string, extra int) error {
	rec, ok := m.record(id)
	if !ok {
		return status.Errorf(codes.NotFound, "no Kubernetes connection %q", id)
	}
	used := extra
	for _, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.KubeCluster == rec.ID && inst.KubeKey != "" && inst.Status != fleet.StatusDeleting {
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

// automationLimits are connection id's limits on automation runs (its
// name, fire interval and per-agent run limit); ok is false without the
// connection.
func (m *kubeManager) automationLimits(id string) (clusterLimits, bool) {
	rec, ok := m.record(id)
	if !ok {
		return clusterLimits{}, false
	}
	return clusterLimits{name: rec.GetDisplayName(), interval: rec.AutomationInterval(), maxPerAgent: rec.AutomationRunLimit()}, true
}

func orDefaultString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
