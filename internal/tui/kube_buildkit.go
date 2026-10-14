package tui

import (
	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// kube_buildkit.go is what a fleet with shared Buildkit on is told of the
// clusters it runs on: the fleet's Buildkit toggle decides on every backend,
// and on a cluster the server runs in the connection's namespace, which the
// edit-fleet dialog's Buildkit row and the New instance dialog's Deploy row
// say, as they say why a cluster does not run it.

// buildkitClusterNote is the note for a fleet with shared Buildkit on that
// runs on the cluster of st: why the cluster does not run it (its Allow
// shared Buildkit is off, its namespace's admission denies the server's pod),
// or, on a cluster that is not local, that it will run in the namespace. ""
// for a local cluster that runs it, and for an unknown one.
func buildkitClusterNote(st *fleetgrpc.KubeClusterStatus) string {
	if st == nil {
		return ""
	}
	c, caps := st.GetCluster(), st.GetCapabilities()
	name := c.DisplayNameOrID()
	switch {
	case c.AllowSharedBuildkit != nil && !c.GetAllowSharedBuildkit():
		return name + ": Allow shared Buildkit is off there; instances there build without it"
	case caps != nil && !caps.GetBuildkitAdmitted() && caps.GetBuildkitReason() != "":
		return name + ": shared Buildkit unavailable: the namespace's admission denies its pod (use a sysbox runtime class)"
	case caps == nil || !caps.GetLocalCluster():
		return name + ": shared Buildkit will run in the namespace"
	}
	return ""
}

// buildkitClusterLines are the Buildkit row's notes, one for each cluster
// the fleet's instances run on, while the row is on.
func (fleetPage *fleetPage) buildkitClusterLines(m *model) []string {
	if m.st == nil {
		return nil
	}
	f, ok := m.st.Fleets[fleetPage.dlg.fleet]
	if !ok || f == nil {
		return nil
	}
	seen := map[string]bool{}
	var lines []string
	for _, inst := range f.Instances {
		if inst == nil || inst.Backend != fleet.BackendKubernetes || inst.KubeCluster == "" || seen[inst.KubeCluster] {
			continue
		}
		seen[inst.KubeCluster] = true
		if note := buildkitClusterNote(m.kubeStatus(inst.KubeCluster)); note != "" {
			lines = append(lines, "      "+warnTextStyle.Render("⚠ "+note))
		}
	}
	return lines
}

// deployBuildkitNote is the Deploy row's note for target, when the fleet the
// instance goes to has shared Buildkit on.
func deployBuildkitNote(m *model, fleetName string, target fleet.DeployTarget) string {
	if target.KubeCluster == "" || m.st == nil {
		return ""
	}
	f, ok := m.st.Fleets[fleetName]
	if !ok || f == nil || !f.Settings.BuildkitServer {
		return ""
	}
	return buildkitClusterNote(m.kubeStatus(target.KubeCluster))
}
