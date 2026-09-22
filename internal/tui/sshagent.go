package tui

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/BenjaminBenetti/fleet-man/internal/agentfwd"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// sshagent.go is the TUI's side of SSH-agent forwarding. The TUI runs on the
// machine the human sits at — where their ssh-agent is — so while it is
// connected to a registered remote fleet with [ agent: on ], it provides
// that agent to the remote daemon (internal/agentfwd.Run): the
// remote's git clones and its instances then use the user's keys, like
// `ssh -A`, for exactly as long as the TUI stays connected. The Fleet Armada
// settings rows carry the per-remote toggle and show the provider's state.

// agentCtl owns the provider goroutine. Like micCtl it is package state rather
// than model state: the goroutine outlives any one Update.
var agentCtl struct {
	mu      sync.Mutex
	parent  context.Context
	program *tea.Program
	cancel  context.CancelFunc // non-nil while a provider goroutine is running
	target  string             // the remote URL the running provider serves
	// gen counts provider STARTS; every status message carries the gen of the
	// goroutine that sent it, and the model drops a superseded provider's.
	gen int
}

// startAgentControl arms agentCtl for the TUI's lifetime. Cancelling parent
// stops the provider. Until this runs (tests) every agentCtl call is a no-op.
func startAgentControl(parent context.Context, program *tea.Program) {
	agentCtl.mu.Lock()
	defer agentCtl.mu.Unlock()
	agentCtl.parent = parent
	agentCtl.program = program
}

// agentForwardTarget is the remote the TUI should provide its agent to: the
// current connection, if it is a registered remote with forwarding on.
func (m *model) agentForwardTarget() string {
	current := armadaCurrentKey()
	if current == "" {
		return ""
	}
	for _, r := range m.armadaRemotes {
		if r.URL == current && r.ForwardAgent {
			return current
		}
	}
	return ""
}

// syncAgentProvider converges the provider on the current connection and the
// registry: running against the current remote iff forwarding is on for it.
// Idempotent — called wherever the registry or the connection changes. The
// `fleet shell` children this TUI spawns provide too, but theirs yield: the
// daemon tries them only after this non-yielding one, so they never push it
// to standby.
func (m *model) syncAgentProvider() {
	target := m.agentForwardTarget()
	agentCtl.mu.Lock()
	defer agentCtl.mu.Unlock()
	if agentCtl.parent == nil {
		return
	}
	if agentCtl.cancel != nil && agentCtl.target == target {
		return
	}
	if agentCtl.cancel != nil {
		agentCtl.cancel()
		agentCtl.cancel = nil
		agentCtl.target = ""
	}
	if target == "" {
		m.agentStatus = agentfwd.Status{}
		return
	}
	ctx, cancel := context.WithCancel(agentCtl.parent)
	agentCtl.cancel = cancel
	agentCtl.target = target
	agentCtl.gen++
	m.agentStatus = agentfwd.Status{}
	go runAgentProviderFn(ctx, agentCtl.program, agentCtl.gen)
}

// agentProviderExited is a provider goroutine's last act: if it is still the
// current one, free the slot so a later sync can start afresh (it can end on
// its own — a daemon that predates the RPC).
func agentProviderExited(gen int) {
	agentCtl.mu.Lock()
	defer agentCtl.mu.Unlock()
	if agentCtl.gen == gen && agentCtl.cancel != nil {
		agentCtl.cancel()
		agentCtl.cancel = nil
		agentCtl.target = ""
	}
}

// agentGen is the generation of the current (or most recent) provider.
func agentGen() int {
	agentCtl.mu.Lock()
	defer agentCtl.mu.Unlock()
	return agentCtl.gen
}

// runAgentProviderFn is the provider goroutine's entry point; a var so tests
// can observe start/stop bookkeeping without dialing a daemon.
var runAgentProviderFn = runAgentProvider

// agentStatusMsg carries a provider status change into the bubbletea loop,
// stamped with the generation of the provider that sent it.
type agentStatusMsg struct {
	status agentfwd.Status
	gen    int
}

// runAgentProvider dials the current endpoint and holds the SSHAgent stream
// until ctx is cancelled. agentfwd.Run reconnects the stream itself; the loop
// here only covers the dial, which can fail while a remote is coming up.
func runAgentProvider(ctx context.Context, program *tea.Program, gen int) {
	report, flush := newLatestForwarder(func(status agentfwd.Status) {
		program.Send(agentStatusMsg{status: status, gen: gen})
	})
	defer flush()
	defer agentProviderExited(gen)

	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		conn, err := fleetclient.Dial(ctx)
		if err != nil {
			report(agentfwd.Status{State: agentfwd.StateConnecting})
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 10*time.Second)
			continue
		}
		agentfwd.Run(ctx, conn.Service(), "tui", report)
		conn.Close()
		return
	}
}

// --- settings page --------------------------------------------------------------

// toggleArmadaAgent flips [ agent: on ] on remote idx and saves the registry;
// the save result re-syncs the provider.
func (settingsPage *settingsPage) toggleArmadaAgent(m *model, idx int) tea.Cmd {
	if idx < 0 || idx >= len(m.armadaRemotes) || settingsPage.armadaBusy {
		return nil
	}
	settingsPage.armadaBusy = true
	next := slices.Clone(m.armadaRemotes)
	next[idx].ForwardAgent = !next[idx].ForwardAgent
	return saveArmadaCmd(next, armadaActionAgent, idx)
}

// agentToggleSavedMessage is the status line after a toggle save.
func (m *model) agentToggleSavedMessage(idx int) string {
	if idx < 0 || idx >= len(m.armadaRemotes) {
		return ""
	}
	remote := m.armadaRemotes[idx]
	name := m.armadaNameFor(remote.URL)
	if !remote.ForwardAgent {
		return "SSH agent forwarding off for " + name
	}
	if remote.URL != armadaCurrentKey() {
		return "SSH agent forwarding on for " + name + " — your agent goes with you while you are connected to it"
	}
	return "SSH agent forwarding on for " + name
}

// renderArmadaAgentButton renders a remote row's [ agent: on/off ] toggle,
// highlighted when the row's sub-cursor is on it.
func (settingsPage *settingsPage) renderArmadaAgentButton(remoteOn, rowActive bool) string {
	label := "[ agent: off ]"
	if remoteOn {
		label = "[ agent: on ]"
	}
	if rowActive && settingsPage.armadaAgentFocused {
		return selectedStyle.Render(label)
	}
	if remoteOn {
		return statusRunningStyle.Render(label)
	}
	return dimStyle.Render(label)
}

// armadaAgentStatusValue describes the provider for the remote the TUI is
// connected to ("" for any other row: its toggle only matters once connected).
func armadaAgentStatusValue(m *model, url string, remoteOn bool) string {
	if !remoteOn || url != armadaCurrentKey() {
		return ""
	}
	st := m.agentStatus
	switch st.State {
	case agentfwd.StateActive:
		return statusRunningStyle.Render("forwarding") + " " + dimStyle.Render(agentUsesText(st.Uses))
	case agentfwd.StateStandby:
		// Another TUI (on this machine or another) attached after this one:
		// a CLI command's provider yields, so it never puts this one here.
		// Not idle: the relay falls through to this agent whenever the newer
		// client cannot answer (it has no agent), so its uses still count.
		value := dimStyle.Render("standing by — a newer client is attached")
		if st.Uses > 0 {
			value += " " + dimStyle.Render(agentUsesText(st.Uses))
		}
		return value
	case agentfwd.StateNoAgent:
		return statusCreatingStyle.Render("no local agent") + " " + dimStyle.Render(st.Detail)
	case agentfwd.StateRefused:
		return statusCreatingStyle.Render("refused") + " " + dimStyle.Render(st.Detail)
	case agentfwd.StateUnsupported:
		return statusCreatingStyle.Render("unsupported") + " " + dimStyle.Render("the remote fleet is too old — update it")
	default:
		return dimStyle.Render("connecting…")
	}
}

// agentUsesText words a provider's use count for the settings row.
func agentUsesText(uses int) string {
	switch {
	case uses == 1:
		return "used once"
	case uses > 1:
		return fmt.Sprintf("used %d times", uses)
	default:
		return "no uses yet"
	}
}
