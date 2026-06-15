package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// copyconfirm.go gates delegated in-instance `fc` copies behind a host-side
// confirmation. A file.copy control envelope lets an in-container process drive a
// copy that touches the human's machine — reading a host file into the instance
// or writing one out. Anything inside the container (including untrusted code or
// a coding agent) can send one, so before performing a copy that touches a host
// path the TUI asks the human to allow it. "Allow for session" remembers the
// originating instance in memory, so repeated copies from a box the human trusts
// don't re-prompt while the TUI stays open.

// copyRequest is one delegated copy awaiting (or cleared for) host-side action.
type copyRequest struct {
	fleet, instance, src, dst string
}

func (r copyRequest) instanceKey() string { return r.fleet + "/" + r.instance }

// copyTouchesHost reports whether either endpoint is a path on this (the host)
// machine — the only case that needs confirmation. A copy purely between
// instances never reads or writes the human's disk.
func copyTouchesHost(src, dst string) bool {
	return fleetclient.ParseCopyEndpoint(src).Kind == fleetclient.CopyLocal ||
		fleetclient.ParseCopyEndpoint(dst).Kind == fleetclient.CopyLocal
}

// copyConfirmShowing reports whether a host-copy confirmation is pending.
func (m *model) copyConfirmShowing() bool {
	return len(m.pendingCopyConfirms) > 0
}

// requestCopy decides what to do with a delegated copy: run it now when it does
// not touch the host, or the originating instance is already session-allowed;
// otherwise queue it for confirmation. Returns the command to run (nil while a
// request is queued for the human to confirm).
func (m *model) requestCopy(req copyRequest) tea.Cmd {
	if !copyTouchesHost(req.src, req.dst) || m.copySessionAllow[req.instanceKey()] {
		return m.startConfirmedCopy(req)
	}
	m.pendingCopyConfirms = append(m.pendingCopyConfirms, req)
	return nil
}

// resolveCopyConfirm handles a keypress while the confirmation is showing: allow
// once, allow for the session, or deny (any other key is swallowed). Returns the
// command to run, if any.
func (m *model) resolveCopyConfirm(key string) tea.Cmd {
	if !m.copyConfirmShowing() {
		return nil
	}
	req := m.pendingCopyConfirms[0]
	switch key {
	case "a", "y", "enter":
		m.pendingCopyConfirms = m.pendingCopyConfirms[1:]
		return m.startConfirmedCopy(req)
	case "s":
		m.copySessionAllow[req.instanceKey()] = true
		// Run this one plus every other queued request from the now-trusted
		// instance; keep the rest queued for their own confirmation.
		cmds := []tea.Cmd{m.startConfirmedCopy(req)}
		remaining := append([]copyRequest(nil), m.pendingCopyConfirms[1:]...)
		m.pendingCopyConfirms = nil
		for _, r := range remaining {
			if r.instanceKey() == req.instanceKey() {
				cmds = append(cmds, m.startConfirmedCopy(r))
			} else {
				m.pendingCopyConfirms = append(m.pendingCopyConfirms, r)
			}
		}
		return tea.Batch(cmds...)
	case "d", "n", "esc":
		m.pendingCopyConfirms = m.pendingCopyConfirms[1:]
		m.message = fmt.Sprintf("Denied copy %s -> %s from %s", req.src, copyDstLabel(req.dst), req.instanceKey())
		return nil
	}
	return nil
}

// startConfirmedCopy sets the status line and returns the background copy command.
func (m *model) startConfirmedCopy(req copyRequest) tea.Cmd {
	m.message = fmt.Sprintf("Copying %s -> %s...", req.src, copyDstLabel(req.dst))
	return copyForInstanceCmd(req.fleet, req.instance, req.src, req.dst)
}

// copyConfirmHostEffects describes, for the human, which host paths a pending
// copy would read or write — the whole reason the prompt exists.
func copyConfirmHostEffects(req copyRequest) []string {
	var effects []string
	if fleetclient.ParseCopyEndpoint(req.src).Kind == fleetclient.CopyLocal {
		effects = append(effects, "reads "+req.src+" on THIS machine")
	}
	switch {
	case req.dst == "":
		effects = append(effects, "writes to your downloads folder on THIS machine")
	case fleetclient.ParseCopyEndpoint(req.dst).Kind == fleetclient.CopyLocal:
		effects = append(effects, "writes "+req.dst+" on THIS machine")
	}
	return effects
}

// viewCopyConfirm renders the centered confirmation overlay for the first
// pending request.
func (m model) viewCopyConfirm() string {
	req := m.pendingCopyConfirms[0]

	boxWidth := 64
	if m.width > 0 && boxWidth > m.width-4 {
		boxWidth = m.width - 4
	}
	boxWidth = max(boxWidth, 24)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("203")).
		Padding(1, 2).
		Width(boxWidth)

	title := dialogTitle.Render("⚠ Copy request from " + req.instanceKey())
	path := dialogLabel.Render(req.src + "  →  " + copyDstLabel(req.dst))
	effects := dialogHint.Render(strings.Join(copyConfirmHostEffects(req), "\n"))
	hint := dialogLabel.Render("[a]llow once    [s] allow for session    [d]eny")

	content := title + "\n\n" + path + "\n" + effects + "\n\n" + hint
	if extra := len(m.pendingCopyConfirms) - 1; extra > 0 {
		content += "\n" + dialogHint.Render(fmt.Sprintf("(%d more request(s) queued)", extra))
	}

	width, height := m.width, m.height
	if width <= 0 {
		width = boxWidth + 4
	}
	if height <= 0 {
		height = lipgloss.Height(box.Render(content))
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box.Render(content))
}
