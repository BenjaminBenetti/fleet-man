package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// automation_login.go shows the daemon's automation status on the agent rows
// (Watch Event.automation_status): which agents the login check refuses, and
// how many runs each has running and queued. A refused agent's row offers
// `L` to log in where it runs: the tool opens in a session on a running
// instance of the fleet on the agent's cluster, created first when none
// runs. Logins are per location, so that one login serves every run there.

// automationStatusMsg carries the daemon's pushed automation status.
type automationStatusMsg struct {
	status *fleetgrpc.AutomationStatus
	gen    int
}

// handleAutomationStatusMsg stores a pushed status and redraws the rows.
func (m *model) handleAutomationStatusMsg(msg automationStatusMsg) {
	if msg.gen != m.watchGen {
		return
	}
	m.automationStatus = map[string]*fleetgrpc.AutomationAgentStatus{}
	for _, a := range msg.status.GetAgents() {
		m.automationStatus[a.GetFleet()+"\x00"+a.GetAgent()] = a
	}
	if m.fleetPage != nil {
		m.fleetPage.buildRows(m)
	}
}

// automationAgentStatus is the pushed status of a fleet's agent, or nil.
func (m *model) automationAgentStatus(fleetName, agentName string) *fleetgrpc.AutomationAgentStatus {
	return m.automationStatus[fleetName+"\x00"+agentName]
}

// agentStatusParts are the agent row's notes from the pushed status: its
// runs, and the login it waits for.
func agentStatusParts(st *fleetgrpc.AutomationAgentStatus) []string {
	var parts []string
	if n := st.GetRunning(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d running", n))
	}
	if n := st.GetQueued(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d queued", n))
	}
	if login := st.GetLogin(); login != "" {
		parts = append(parts, "⚠ "+login+" (L)")
	}
	return parts
}

// pendingLogin is a Log in action waiting for the instance it created.
type pendingLogin struct {
	fleet, instance, tool string
	since                 time.Time
}

// pendingLoginTimeout is how long a Log in action waits for its instance.
const pendingLoginTimeout = 30 * time.Minute

// loginSessionMsg reports the login session a Log in action started.
type loginSessionMsg struct {
	ref     InstanceRef
	session string
	tool    string
	err     error
}

// logInAgent is the agent row's Log in action (L).
func (fleetPage *fleetPage) logInAgent(m *model, fleetName string, idx int) tea.Cmd {
	f, ok := m.st.Fleets[fleetName]
	if !ok {
		return nil
	}
	a := agentAt(f, idx)
	if a == nil {
		return nil
	}
	st := m.automationAgentStatus(fleetName, a.Name)
	if st.GetLogin() == "" {
		m.message = fmt.Sprintf("Agent %q needs no login where it runs", a.Name)
		return nil
	}
	tool, ok := fleet.LoginToolNamed(st.GetLoginTool())
	if !ok {
		m.message = st.GetLogin()
		return nil
	}
	target := a.DeployTarget()
	if inst := runningInstanceOn(f, target); inst != nil {
		m.message = fmt.Sprintf("Opening %s in %s to log in", tool.Name, inst.Name)
		return loginSessionCmd(InstanceRef{Fleet: fleetName, Instance: inst.Name}, tool)
	}
	name := freeInstanceName(f, "login-"+tool.Tool)
	fleetPage.pendingLogin = &pendingLogin{fleet: fleetName, instance: name, tool: tool.Tool, since: time.Now()}
	m.message = fmt.Sprintf("Creating %s on %s to log in to %s; the login opens once it runs", name, deployTargetLabel(m, target), tool.Name)
	return createInstanceCmd(fleetName, name, "", "", "", target)
}

// pendingLoginFor is the Log in action waiting for instance of fleetName,
// or nil.
func (fleetPage *fleetPage) pendingLoginFor(fleetName, instance string) *pendingLogin {
	if fleetPage == nil {
		return nil
	}
	if p := fleetPage.pendingLogin; p != nil && p.fleet == fleetName && p.instance == instance {
		return p
	}
	return nil
}

// checkPendingLogin opens the login a Log in action waits for once its
// instance runs (or gives up when it failed or took too long).
func (fleetPage *fleetPage) checkPendingLogin(m *model) tea.Cmd {
	p := fleetPage.pendingLogin
	if p == nil {
		return nil
	}
	var inst *fleet.Instance
	if f, ok := m.st.Fleets[p.fleet]; ok {
		inst, _ = f.GetInstance(p.instance)
	}
	switch {
	case inst != nil && inst.Status == fleet.StatusRunning:
		fleetPage.pendingLogin = nil
		tool, _ := fleet.LoginToolNamed(p.tool)
		m.message = fmt.Sprintf("Opening %s in %s to log in", tool.Name, inst.Name)
		return loginSessionCmd(InstanceRef{Fleet: p.fleet, Instance: inst.Name}, tool)
	case inst != nil && inst.Status == fleet.StatusFailed:
		fleetPage.pendingLogin = nil
		m.message = fmt.Sprintf("Could not log in: %s failed: %s", inst.Name, inst.Error)
	case time.Since(p.since) > pendingLoginTimeout:
		fleetPage.pendingLogin = nil
	}
	return nil
}

// runningInstanceOn is a running instance of f on target, or nil. An
// automation run's instance is never one: the scheduler reaps it once its
// agent idles, login session and all.
func runningInstanceOn(f *fleet.Fleet, target fleet.DeployTarget) *fleet.Instance {
	for _, inst := range f.Instances {
		if inst.Status == fleet.StatusRunning && !inst.Automated && inst.Backend == target.Backend && inst.KubeCluster == target.KubeCluster {
			return inst
		}
	}
	return nil
}

// freeInstanceName is base, or base-N, whichever no instance of f has.
func freeInstanceName(f *fleet.Fleet, base string) string {
	name := base
	for n := 2; ; n++ {
		if _, err := f.GetInstance(name); err != nil {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
}

// loginSessionScript starts tool's login command in session (unless it
// already runs), in an interactive bash as the agents run, which stays
// when the command ends.
func loginSessionScript(session string, tool fleet.LoginTool) string {
	launch := "bash -ic " + shQuote(tool.LoginCommand+"; exec bash -i")
	return tmuxEnsureInstalled + fmt.Sprintf("tmux has-session -t %s 2>/dev/null || tmux new-session -d -s %s %s 2>&1",
		shQuote("="+session), shQuote(session), shQuote(launch))
}

// loginSessionCmd starts the login session in the instance.
func loginSessionCmd(ref InstanceRef, tool fleet.LoginTool) tea.Cmd {
	session := ResolveSessionName(ref.Instance, "login-"+tool.Tool)
	return func() tea.Msg {
		_, err := runSessionScript(ref, loginSessionScript(session, tool))
		return loginSessionMsg{ref: ref, session: session, tool: tool.Tool, err: err}
	}
}

// handleLoginSessionMsg attaches the user to the login session.
func (m *model) handleLoginSessionMsg(msg loginSessionMsg) tea.Cmd {
	if msg.err != nil {
		m.message = fmt.Sprintf("Could not start the login session: %v", msg.err)
		return nil
	}
	shellCmd := ShellCommandForSession(m.config, msg.session, m.width, m.height, false)
	cmd, err := attachExecCmd(msg.ref.Fleet, msg.ref.Instance, shellCmd)
	if err != nil {
		m.message = fmt.Sprintf("Could not open the login session: %v", err)
		return nil
	}
	tool, _ := fleet.LoginToolNamed(msg.tool)
	banner := renderGradient(nameToBanner(msg.ref.Instance))
	banner += "\n  " + dimStyle.Render(strings.TrimSpace(fmt.Sprintf("log in to %s here; ctrl+q/ctrl+o to detach (session persists)", tool.Name)))
	return execProcess(execWithBannerCmd(banner, cmd), func(err error) tea.Msg { return execDoneMsg{err} })
}
