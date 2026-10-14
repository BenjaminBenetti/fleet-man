package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	tea "github.com/charmbracelet/bubbletea"
)

// kubeAgentModel is the automation test model with connection dev (ready)
// pushed, and a daemon that lists every backend and dev and remembers def.
func kubeAgentModel(t *testing.T, def fleet.DeployTarget, options ...fleet.DeployTargetOption) (*model, *fleetPage) {
	t.Helper()
	m, fp := newAutomationModel(t)
	m.kubeClusters = &fleetgrpc.KubeClusters{Clusters: []*fleetgrpc.KubeClusterStatus{
		kubeSt("dev-abc234", "dev", fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY),
	}}
	stubListDeployTargets(t, deployTargetsReply(def, options...), nil)
	return m, fp
}

var devClusterTarget = fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "dev-abc234"}

// A new agent opens on the daemon's remembered target (here a cluster, not
// this TUI's config default), its Backend row cycles every target the
// daemon lists, and the agent saves with the cluster it shows.
func TestNewAgentOpensOnTheRememberedCluster(t *testing.T) {
	m, fp := kubeAgentModel(t, devClusterTarget,
		available(devcontainerTarget), unavailable(coderTarget), available(codespacesTarget), available(devClusterTarget))
	fp.openAddAgentDialog(m, "alpha")
	st := &fp.agentDlg
	if st.target != devClusterTarget {
		t.Fatalf("new agent opened on %v, want the remembered cluster", st.target)
	}
	if view := flatText(fp.renderAutomationAgentDialog(m)); !strings.Contains(view, "Backend: [ ☸ dev ]") {
		t.Fatalf("Backend row should name the cluster:\n%s", view)
	}

	// Every listed target cycles, the unavailable one too (an agent fires
	// later), with the daemon's reason beside it.
	st.row = agentRowBackend
	var seen []fleet.DeployTarget
	for range 4 {
		fp.updateAutomationAgent(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
		seen = append(seen, st.target)
		if st.target == coderTarget {
			if view := flatText(fp.renderAutomationAgentDialog(m)); !strings.Contains(view, "coder CLI not found where fleetd runs") {
				t.Errorf("an unavailable target should say why:\n%s", view)
			}
		}
	}
	want := []fleet.DeployTarget{devcontainerTarget, coderTarget, codespacesTarget, devClusterTarget}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("Backend row cycled %v, want %v", seen, want)
		}
	}

	st.name = "k8s-agent"
	fp.saveAutomationAgent(m)
	agents := m.st.Fleets["alpha"].Settings.Agents
	if len(agents) != 1 || agents[0].DeployTarget() != devClusterTarget {
		t.Fatalf("saved agents %+v, want one on the cluster", agents)
	}
	// The automation view's agent row names where it runs.
	if got := agentSummary(m, m.st.Fleets["alpha"], agents[0]); !strings.HasPrefix(got, "☸ dev") {
		t.Fatalf("agent row summary = %q, want it to start with ☸ dev", got)
	}
}

// An agent whose connection was forgotten shows "(missing)" on its row and
// in its dialog, which saves nothing until another target is picked.
func TestAgentOnAForgottenClusterAsksForATarget(t *testing.T) {
	m, fp := kubeAgentModel(t, devcontainerTarget,
		available(devcontainerTarget), available(coderTarget), available(codespacesTarget), available(devClusterTarget))
	gone := fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "prod-xyz567"}
	f := m.st.Fleets["alpha"]
	f.Settings.Agents = []fleet.Agent{{Name: "k", Backend: gone.Backend, KubeCluster: gone.KubeCluster, Command: "claude"}}
	var saves []fleet.FleetSettings
	setFleetSettingsRemote = func(_ string, s fleet.FleetSettings) ([]string, error) {
		saves = append(saves, s)
		return nil, nil
	}

	if got := agentSummary(m, f, f.Settings.Agents[0]); !strings.HasPrefix(got, "☸ prod-xyz567 (missing)") {
		t.Fatalf("agent row summary = %q, want the missing cluster named", got)
	}
	fp.openEditAgentDialog(m, "alpha", 0)
	st := &fp.agentDlg
	if st.target != gone {
		t.Fatalf("edit dialog target %v, want the agent's own", st.target)
	}
	view := flatText(fp.renderAutomationAgentDialog(m))
	for _, want := range []string{"Backend: [ ☸ prod-xyz567 (missing) ]", "is no longer connected: pick another target"} {
		if !strings.Contains(view, want) {
			t.Fatalf("edit dialog of an agent on a forgotten cluster lacks %q:\n%s", want, view)
		}
	}

	// Editing the command saves nothing while the target is gone.
	st.row = agentRowCommand
	fp.agentRowEnter(m)
	st.input.SetValue("codex")
	fp.updateAutomationAgent(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(saves) != 0 {
		t.Fatalf("an edit saved %+v with the target still gone", saves)
	}
	if err := fp.agentFormError(m); err == nil || !strings.Contains(err.Error(), "no longer connected") {
		t.Fatalf("agentFormError = %v, want the missing target", err)
	}

	// Moving it to a live target saves at once, with the command.
	st.row = agentRowBackend
	fp.updateAutomationAgent(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if st.target != devcontainerTarget || len(saves) != 1 {
		t.Fatalf("target %v after one step, %d saves; want devcontainer, saved", st.target, len(saves))
	}
	a := saves[0].Agents[0]
	if a.DeployTarget() != devcontainerTarget || a.Command != "codex" {
		t.Fatalf("saved %+v, want devcontainer with the edited command", a)
	}
	if st.errMsg != "" {
		t.Fatalf("errMsg %q after a good save", st.errMsg)
	}
}

// Without the daemon's list (an old daemon), the dialog still offers every
// backend and the pushed connections, opening on this TUI's config default.
func TestAgentTargetsWithoutTheDaemon(t *testing.T) {
	m, _ := kubeAgentModel(t, fleet.DeployTarget{})
	stubListDeployTargets(t, nil, errString("unreachable"))
	m.config = nil
	options, def := agentTargets(m, "alpha", fleet.DeployTarget{})
	var got []fleet.DeployTarget
	for _, o := range options {
		got = append(got, o.Target)
	}
	want := []fleet.DeployTarget{devcontainerTarget, coderTarget, codespacesTarget, devClusterTarget}
	if len(got) != len(want) {
		t.Fatalf("targets %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targets %v, want %v", got, want)
		}
	}
	if def != devcontainerTarget {
		t.Fatalf("default %v, want devcontainer", def)
	}
}

// An agent whose own target the daemon does not list for its fleet (Coder
// in a template fleet, which only devcontainer and the clusters can copy)
// stays on the Backend row with why, and that reason is not the forgotten
// connection's: nothing about it was disconnected.
func TestAgentOnAnUnlistedTargetSaysWhy(t *testing.T) {
	m, fp := kubeAgentModel(t, devcontainerTarget, available(devcontainerTarget), available(devClusterTarget))
	f := m.st.Fleets["alpha"]
	f.Remote = "file:///home/me/scratch"
	f.Settings.Agents = []fleet.Agent{{Name: "c", Backend: fleet.BackendCoder, Command: "claude"}}
	fp.openEditAgentDialog(m, "alpha", 0)
	st := &fp.agentDlg
	if st.target != coderTarget || st.errMsg != "" {
		t.Fatalf("edit dialog target %v, errMsg %q; want the agent's own, no error", st.target, st.errMsg)
	}
	view := flatText(fp.renderAutomationAgentDialog(m))
	if !strings.Contains(view, "cannot create this fleet's instances") || strings.Contains(view, "no longer connected") {
		t.Fatalf("an unlisted target should say it cannot take the fleet's instances:\n%s", view)
	}
	// A forgotten cluster keeps its own reason.
	gone := fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: "prod-xyz567"}
	if got := unlistedTargetMessage(m, gone); !strings.Contains(got, "no longer connected") {
		t.Fatalf("unlistedTargetMessage(forgotten cluster) = %q", got)
	}
}
