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
//
// A `fleet open` (fo) is a copy PLUS handing the file to the desktop's opener,
// which is a bigger thing to pre-approve than a copy, so it has its own session
// allowance: an [s] pressed on a plain copy never unlocks unattended opens, while
// an [s] pressed on an open covers both (an open is a superset of a copy).

// copyRequest is one delegated copy awaiting (or cleared for) host-side action.
// open marks a `fleet open` (fo): once the file lands on this machine it is
// also opened with the default application — one more thing the human is
// agreeing to, so the prompt spells it out.
type copyRequest struct {
	fleet, instance, src, dst string
	open                      bool
}

func (r copyRequest) instanceKey() string { return r.fleet + "/" + r.instance }

// verb names the request for status lines: "copy", or "open" for a fleet open.
func (r copyRequest) verb() string {
	if r.open {
		return "open"
	}
	return "copy"
}

// verbs is the plural, for the session-allow hint ("allow copies/opens").
func (r copyRequest) verbs() string {
	if r.open {
		return "opens"
	}
	return "copies"
}

// copyTouchesHost reports whether either endpoint is a path on this (the host)
// machine — the only case that needs confirmation. A copy purely between
// instances never reads or writes the human's disk. The host machine is named
// `host:`; an empty dst (the 1-arg download shorthand → downloads folder) also
// writes here, and a bare local path is treated as host-side defensively (the
// in-instance `fc` rewrites its own this-instance plain paths to `:` self).
func copyTouchesHost(src, dst string) bool {
	return endpointIsHost(src) || endpointIsHost(dst)
}

func endpointIsHost(arg string) bool {
	switch fleetclient.ParseCopyEndpoint(arg).Kind {
	case fleetclient.CopyHost, fleetclient.CopyLocal:
		return true
	}
	return false
}

// copyConfirmShowing reports whether a host-copy confirmation is pending.
func (m *model) copyConfirmShowing() bool {
	return len(m.pendingCopyConfirms) > 0
}

// requestCopy decides what to do with a delegated copy: run it now when it does
// not touch the host, or the originating instance is already session-allowed
// for this kind of request; otherwise queue it for confirmation. Returns the
// command to run (nil while a request is queued for the human to confirm).
func (m *model) requestCopy(req copyRequest) tea.Cmd {
	if !copyTouchesHost(req.src, req.dst) || m.sessionAllowed(req) {
		return m.startConfirmedCopy(req)
	}
	m.pendingCopyConfirms = append(m.pendingCopyConfirms, req)
	return nil
}

// sessionAllowed reports whether the human already cleared this instance for
// the session for this KIND of request: an open needs the open allowance; a
// copy is covered by either (an open allowance implies copies).
func (m *model) sessionAllowed(req copyRequest) bool {
	key := req.instanceKey()
	if req.open {
		return m.openSessionAllow[key]
	}
	return m.copySessionAllow[key] || m.openSessionAllow[key]
}

// allowSession records the human's [s] for req's instance: copies for a copy
// request, copies AND opens for an open request.
func (m *model) allowSession(req copyRequest) {
	key := req.instanceKey()
	m.copySessionAllow[key] = true
	if req.open {
		m.openSessionAllow[key] = true
	}
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
		m.allowSession(req)
		// Run this one plus every other queued request the new allowance now
		// covers; keep the rest queued for their own confirmation.
		cmds := []tea.Cmd{m.startConfirmedCopy(req)}
		remaining := append([]copyRequest(nil), m.pendingCopyConfirms[1:]...)
		m.pendingCopyConfirms = nil
		for _, r := range remaining {
			if m.sessionAllowed(r) {
				cmds = append(cmds, m.startConfirmedCopy(r))
			} else {
				m.pendingCopyConfirms = append(m.pendingCopyConfirms, r)
			}
		}
		return tea.Batch(cmds...)
	case "d", "n", "esc":
		m.pendingCopyConfirms = m.pendingCopyConfirms[1:]
		m.message = fmt.Sprintf("Denied %s %s -> %s from %s", req.verb(), req.src, copyDstLabel(req.dst), req.instanceKey())
		return nil
	}
	return nil
}

// startConfirmedCopy sets the status line and returns the background copy command.
func (m *model) startConfirmedCopy(req copyRequest) tea.Cmd {
	if req.open {
		m.message = fmt.Sprintf("Copying %s -> %s and opening...", req.src, copyDstLabel(req.dst))
	} else {
		m.message = fmt.Sprintf("Copying %s -> %s...", req.src, copyDstLabel(req.dst))
	}
	return copyForInstanceCmd(req)
}

// copyConfirmHostEffects describes, for the human, which host paths a pending
// copy would read or write — and, for a fleet open, that the delivered file is
// then handed to this machine's default application — the whole reason the
// prompt exists.
func copyConfirmHostEffects(req copyRequest) []string {
	var effects []string
	if endpointIsHost(req.src) {
		effects = append(effects, "reads "+req.src+" on THIS machine")
	}
	switch {
	case req.dst == "":
		effects = append(effects, "writes to your downloads folder on THIS machine")
	case endpointIsHost(req.dst):
		effects = append(effects, "writes "+req.dst+" on THIS machine")
	}
	if req.open {
		effects = append(effects, "opens it with THIS machine's default application")
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

	title := dialogTitle.Render("⚠ " + capitalize(req.verb()) + " request from " + req.instanceKey())
	path := dialogLabel.Render(req.src + "  →  " + copyDstLabel(req.dst))
	effects := dialogHint.Render(strings.Join(copyConfirmHostEffects(req), "\n"))
	hint := dialogLabel.Render("[a]llow once    [s] allow " + req.verbs() + " for session    [d]eny")

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

// capitalize upper-cases the first letter of an ASCII word.
func capitalize(word string) string {
	if word == "" {
		return word
	}
	return strings.ToUpper(word[:1]) + word[1:]
}
