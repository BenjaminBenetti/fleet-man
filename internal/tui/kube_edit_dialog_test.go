package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// The Edit instance dialog of a cluster instance shows the connection it is
// bound to, read-only, and says when the daemon no longer has it.
func TestEditInstanceShowsItsCluster(t *testing.T) {
	m, _ := kubeTestModel(t, kubeSt("mk-1", "minikube", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY))
	fp := newFleetPage()
	inst := &fleet.Instance{Name: "one", Backend: fleet.BackendKubernetes, KubeCluster: "mk-1", KubeKey: "abcd2345"}
	f := &fleet.Fleet{Name: "alpha", Remote: "git@x:alpha.git", Instances: []*fleet.Instance{inst}}
	m.st.Fleets["alpha"] = f
	m.fleetPage = fp
	fp.rows = []row{{kind: rowInstance, fleetName: "alpha", instance: inst}}

	fp.openEditInstanceDialog(m)
	want := fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "mk-1"}
	if fp.addInst.target != want || fp.addInst.targets != nil {
		t.Fatalf("edit dialog target %v targets %v", fp.addInst.target, fp.addInst.targets)
	}
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyTab})
	if fp.addInst.target != want {
		t.Fatalf("tab moved an existing instance to %v", fp.addInst.target)
	}
	if view := flatText(fp.renderAddInstanceDialog(m)); !strings.Contains(view, "[ ☸ minikube ]") || strings.Contains(view, "(missing)") {
		t.Fatalf("edit dialog of a cluster instance:\n%s", view)
	}

	// The connection was removed (Forget): the instance still names it.
	m.kubeClusters = &fleetgrpc.KubeClusters{}
	if view := flatText(fp.renderAddInstanceDialog(m)); !strings.Contains(view, "[ ☸ mk-1 (missing) ]") {
		t.Fatalf("edit dialog of an instance whose cluster is gone:\n%s", view)
	}
	// Before the daemon has said which connections it has, nothing is
	// called missing.
	m.kubeClusters = nil
	if view := flatText(fp.renderAddInstanceDialog(m)); !strings.Contains(view, "[ ☸ mk-1 ]") {
		t.Fatalf("edit dialog before the connections are known:\n%s", view)
	}
}
