package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	tea "github.com/charmbracelet/bubbletea"
)

// loginAgentModel is the automation view of fleet alpha with agent k on
// connection dev, which the pushed automation status says needs a Claude
// login, with the cursor on k's row.
func loginAgentModel(t *testing.T) (*model, *fleetPage) {
	t.Helper()
	m, fp := kubeAgentModel(t, devcontainerTarget, available(devcontainerTarget), available(devClusterTarget))
	m.st.Fleets["alpha"].Settings.Agents = []fleet.Agent{{Name: "k", Command: fleet.DefaultAgentCommand, Backend: fleet.BackendKubernetes, KubeCluster: "dev-abc234"}}
	fp.toggleAutomationMode(m, "alpha")
	m.handleAutomationStatusMsg(automationStatusMsg{status: &fleetgrpc.AutomationStatus{Agents: []*fleetgrpc.AutomationAgentStatus{{
		Fleet: "alpha", Agent: "k", Login: "log in to Claude once for this fleet on ☸ dev", LoginTool: "claude", Queued: 2, Running: 1,
	}}}, gen: m.watchGen})
	for i, r := range fp.rows {
		if r.kind == rowAgent {
			fp.cursor = i
		}
	}
	return m, fp
}

// The agent row carries the pushed status: its runs and the login its
// firings wait for, with the key that logs in, also in the footer.
func TestAgentRowShowsTheAutomationStatus(t *testing.T) {
	m, fp := loginAgentModel(t)
	got := agentSummary(m, m.st.Fleets["alpha"], m.st.Fleets["alpha"].Settings.Agents[0])
	want := "☸ dev · ← 0 triggers · 1 running · 2 queued · ⚠ log in to Claude once for this fleet on ☸ dev (L)"
	if got != want {
		t.Fatalf("agent row = %q, want %q", got, want)
	}
	if keys := strings.Join(fp.contextualHelpKeysBase(m), " | "); !strings.Contains(keys, "L: log in") {
		t.Fatalf("footer = %q, want L: log in", keys)
	}
	// A status that says nothing more (the login done) drops the notes.
	m.handleAutomationStatusMsg(automationStatusMsg{status: &fleetgrpc.AutomationStatus{}, gen: m.watchGen})
	if got := agentSummary(m, m.st.Fleets["alpha"], m.st.Fleets["alpha"].Settings.Agents[0]); got != "☸ dev · ← 0 triggers" {
		t.Fatalf("agent row after the login = %q", got)
	}
	if keys := strings.Join(fp.contextualHelpKeysBase(m), " | "); strings.Contains(keys, "L: log in") {
		t.Fatalf("footer still offers a login: %q", keys)
	}
	// A status from a daemon the TUI switched away from is dropped.
	m.handleAutomationStatusMsg(automationStatusMsg{status: &fleetgrpc.AutomationStatus{Agents: []*fleetgrpc.AutomationAgentStatus{{Fleet: "alpha", Agent: "k", Queued: 9}}}, gen: m.watchGen - 1})
	if m.automationAgentStatus("alpha", "k") != nil {
		t.Fatal("a stale daemon's status was kept")
	}
}

// L on a refused agent opens its tool's login in a running instance of the
// fleet on the agent's cluster.
func TestLogInOpensTheToolOnARunningInstanceThere(t *testing.T) {
	m, fp := loginAgentModel(t)
	f := m.st.Fleets["alpha"]
	f.Instances = []*fleet.Instance{
		{Name: "elsewhere", Status: fleet.StatusRunning, Backend: fleet.BackendDevcontainer},
		// An automation run there is reaped once its agent idles.
		{Name: "a-run", Status: fleet.StatusRunning, Backend: fleet.BackendKubernetes, KubeCluster: "dev-abc234", Automated: true},
		{Name: "there", Status: fleet.StatusRunning, Backend: fleet.BackendKubernetes, KubeCluster: "dev-abc234"},
	}
	var ran []string
	orig := runInstanceCommand
	runInstanceCommand = func(fleetName, instanceName string, argv []string) (string, int, error) {
		ran = append(ran, fleetName+"/"+instanceName+": "+strings.Join(argv, " "))
		return "", 0, nil
	}
	t.Cleanup(func() { runInstanceCommand = orig })

	cmd := fp.Update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'L'}})
	if cmd == nil {
		t.Fatalf("L did nothing: %q", m.message)
	}
	msg, ok := cmd().(loginSessionMsg)
	if !ok || msg.err != nil || msg.ref.Instance != "there" || msg.session != ResolveSessionName("there", "login-claude") {
		t.Fatalf("L ran %+v", msg)
	}
	if len(ran) != 1 || !strings.HasPrefix(ran[0], "alpha/there: sh -c ") || !strings.Contains(ran[0], "claude; exec bash -i") {
		t.Fatalf("ran %q", ran)
	}
}

// With no instance of the fleet running there, L creates one and opens the
// login once it runs; a create that fails says so instead.
func TestLogInCreatesAnInstanceWhenNoneRunsThere(t *testing.T) {
	m, fp := loginAgentModel(t)
	var created []string
	orig := createInstanceRemote
	createInstanceRemote = func(fleetName, instanceName, _, _ string, target fleet.DeployTarget) error {
		created = append(created, fleetName+"/"+instanceName+" on "+target.String())
		return nil
	}
	t.Cleanup(func() { createInstanceRemote = orig })
	m.st.Fleets["alpha"].Instances = []*fleet.Instance{{Name: "login-claude", Status: fleet.StatusStopped, Backend: fleet.BackendKubernetes, KubeCluster: "dev-abc234"}}

	cmd := fp.Update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'L'}})
	if cmd == nil {
		t.Fatalf("L did nothing: %q", m.message)
	}
	cmd()
	if len(created) != 1 || created[0] != "alpha/login-claude-2 on kubernetes:dev-abc234" {
		t.Fatalf("created %v", created)
	}
	if fp.checkPendingLogin(m) != nil {
		t.Fatal("the login opened before its instance exists")
	}
	inst := &fleet.Instance{Name: "login-claude-2", Status: fleet.StatusCreating, Backend: fleet.BackendKubernetes, KubeCluster: "dev-abc234"}
	m.st.Fleets["alpha"].Instances = append(m.st.Fleets["alpha"].Instances, inst)
	if fp.checkPendingLogin(m) != nil {
		t.Fatal("the login opened while its instance is creating")
	}
	inst.Status = fleet.StatusRunning
	if fp.checkPendingLogin(m) == nil || fp.pendingLogin != nil {
		t.Fatal("the login did not open once its instance ran")
	}

	fp.pendingLogin = &pendingLogin{fleet: "alpha", instance: "login-claude-2", tool: "claude"}
	inst.Status, inst.Error = fleet.StatusFailed, "kubernetes:quota: full"
	if fp.checkPendingLogin(m) != nil || fp.pendingLogin != nil || !strings.Contains(m.message, "kubernetes:quota: full") {
		t.Fatalf("a failed instance: pending %+v, message %q", fp.pendingLogin, m.message)
	}
}

// A Log in whose instance could not even be created waits no more.
func TestLogInStopsWaitingWhenItsCreateIsRefused(t *testing.T) {
	m, fp := loginAgentModel(t)
	m.currentPage = fp
	fp.pendingLogin = &pendingLogin{fleet: "alpha", instance: "login-claude", tool: "claude"}
	next, _ := m.Update(instanceCreateErrMsg{fleet: "alpha", instance: "other", err: errors.New("no")})
	if got := next.(model).fleetPage.pendingLogin; got == nil {
		t.Fatal("another instance's refused create dropped the pending login")
	}
	next, _ = next.(model).Update(instanceCreateErrMsg{fleet: "alpha", instance: "login-claude", err: errors.New("kubernetes:quota: full")})
	if got := next.(model).fleetPage.pendingLogin; got != nil {
		t.Fatalf("pending login after its create was refused = %+v", got)
	}
}
