package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// page_settings_kube.go is the Kubernetes section of Settings: one row per
// cluster connection the daemon holds, its health badge and warnings from
// Watch Event.kube_clusters, the row's [ test ] and [ delete ] buttons, and
// the routing into the context picker, the cluster editor and the disconnect
// dialog (dialog_kube_*.go). Every row, button and field works the same from
// a remote TUI (gateway or Armada SSH) as from a local one.

// kubeDialog is a Kubernetes dialog that takes over the Settings page while
// it is open: the context picker, the cluster editor, or the disconnect
// dialog.
type kubeDialog interface {
	// update handles a key or a message meant for this dialog. handled is
	// false for messages it does not own (another dialog's late answer).
	update(m *model, page *settingsPage, msg tea.Msg) (cmd tea.Cmd, handled bool)
	// view renders the dialog for a terminal of m.width x m.height.
	view(m *model, page *settingsPage) string
}

// kubeTrustLine is who else can reach what a connection holds: the RBAC verbs
// that put someone inside the instances, and those that reach the agent
// logins kept on the fleet share.
func kubeTrustLine() string {
	return kubeWarnStyle.Render(kubeGlyphWarn) + " " + dimStyle.Render(
		"pods/exec, pod create or statefulset patch in a namespace = inside your instances · "+
			"pod create or PVC / volume snapshot access there = your agent logins")
}

// kubeShortContext keeps a long context name (an EKS ARN) to one column.
func kubeShortContext(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	keep := max - 1
	head := keep / 2
	return string(r[:head]) + "…" + string(r[len(r)-(keep-head):])
}

// renderKubeClusterRow renders one connection in width columns: name,
// context · namespace, health, version, what uses it and the row's buttons on
// its first line (the health message shortened to fit, then given in full
// below), then its warnings and any test or login in flight.
func (settingsPage *settingsPage) renderKubeClusterRow(m *model, st *fleetgrpc.KubeClusterStatus, active bool, width int) string {
	c := st.GetCluster()
	const valueCol = 21 // cursor (2) + label (18) + separator (1)
	where := dimStyle.Render(kubeShortContext(c.GetContext(), 24) + " · " + c.GetNamespace())
	instances, agents := kubeUsers(m, c.GetId())
	var uses []string
	if len(instances) > 0 {
		uses = append(uses, countNoun(len(instances), "instance"))
	}
	if len(agents) > 0 {
		uses = append(uses, countNoun(len(agents), "agent"))
	}
	after := ""
	if v := st.GetCapabilities().GetVersion(); v != "" {
		after += " " + dimStyle.Render(v)
	}
	if len(uses) > 0 {
		after += "  " + dimStyle.Render(strings.Join(uses, " · "))
	}
	after += "  " + kubeButton("[ test ]", active && settingsPage.kubeTestFocused) +
		" " + kubeButton("[ delete ]", active && settingsPage.kubeDeleteFocused)

	indent := "\n" + strings.Repeat(" ", valueCol)
	var below string
	run := m.kubeRuns[kubeTestKey(c.GetId())]
	var health string
	if run.running() {
		health = m.spinner.View() + " " + dimStyle.Render("testing…") + " " + kubeCheckTrail(run.checks, 4)
	} else {
		room := width - valueCol - lipgloss.Width(where) - 2 - lipgloss.Width(after)
		health = kubeBadge(st)
		if lipgloss.Width(health) > room {
			health = kubeBadgeShort(st, max(12, room))
			below += indent + dimStyle.Render(st.GetMessage())
		}
	}
	value := where + "  " + health + after + below
	if login := m.kubeLogins[c.GetId()]; login != nil && !login.done {
		value += indent + m.spinner.View() + " " + dimStyle.Render("login on fleetd: "+kubeDeviceCodeLine(login.code))
	}
	if run != nil && run.done && run.err != nil {
		value += indent + kubeErrStyle.Render(kubeGlyphFailed+" test: "+kubeTestError(run.err))
	}
	for _, w := range st.GetWarnings() {
		value += indent + kubeWarningLine(w)
	}
	label := kubeGlyphCluster + " " + kubeShortContext(c.DisplayNameOrID(), 15)
	return kubeHangingIndent(settingsPage.renderSettingsRow(m, active, label, value), width, valueCol)
}

// kubeHangingIndent wraps each line of s to width, indenting the lines a
// wrap makes by indent columns so they stay in the value column.
func kubeHangingIndent(s string, width, indent int) string {
	if width <= indent+10 {
		return s
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if lipgloss.Width(line) <= width {
			out = append(out, line)
			continue
		}
		// The first line keeps the full width; what follows is wrapped again
		// to the narrower value column. Words are kept whole where they fit.
		parts := strings.Split(ansi.Wrap(line, width, ""), "\n")
		out = append(out, parts[0])
		rest := strings.TrimLeft(strings.Join(parts[1:], " "), " ")
		for _, part := range strings.Split(ansi.Wrap(rest, width-indent, ""), "\n") {
			out = append(out, strings.Repeat(" ", indent)+part)
		}
	}
	return strings.Join(out, "\n")
}

// kubeButton renders one of a row's buttons, highlighted when focused.
func kubeButton(label string, focused bool) string {
	if focused {
		return selectedStyle.Render(label)
	}
	return dimStyle.Render(label)
}

// resetKubeRowFocus puts a cluster row's sub-cursor back on the row.
func (settingsPage *settingsPage) resetKubeRowFocus() {
	settingsPage.kubeTestFocused = false
	settingsPage.kubeDeleteFocused = false
}

// stepKubeRowFocus moves a cluster row's sub-cursor: the row → [ test ] →
// [ delete ] (dir 1), and back (dir -1).
func (settingsPage *settingsPage) stepKubeRowFocus(dir int) {
	pos := 0
	switch {
	case settingsPage.kubeDeleteFocused:
		pos = 2
	case settingsPage.kubeTestFocused:
		pos = 1
	}
	pos = max(0, min(2, pos+dir))
	settingsPage.kubeTestFocused = pos == 1
	settingsPage.kubeDeleteFocused = pos == 2
}

// enterKubeClusterRow acts on enter on the i-th connection's row: [ test ]
// runs the preflight now, [ delete ] opens the disconnect dialog, and the
// row itself opens the cluster editor.
func (settingsPage *settingsPage) enterKubeClusterRow(m *model, i int) tea.Cmd {
	st := m.kubeStatusAt(i)
	if st == nil {
		return nil
	}
	id := st.GetCluster().GetId()
	switch {
	case settingsPage.kubeTestFocused:
		m.message = fmt.Sprintf("Testing %s %s…", kubeGlyphCluster, st.GetCluster().DisplayNameOrID())
		return m.startKubeTest(kubeTestKey(id), id, &fleetgrpc.TestKubeClusterRequest{Id: id})
	case settingsPage.kubeDeleteFocused:
		settingsPage.resetKubeRowFocus()
		settingsPage.kubeDialog = newKubeRemoveDialog(m, id)
		return nil
	}
	editor := newKubeEditor(id)
	editor.gen = m.watchGen
	settingsPage.kubeDialog = editor
	return editor.loadUsage()
}

// cursorToKubeCluster puts the cursor on connection id's row.
func (settingsPage *settingsPage) cursorToKubeCluster(m *model, id string) {
	for i, st := range m.kubeStatuses() {
		if st.GetCluster().GetId() == id {
			settingsPage.cursorToItem(m, settingsItemKubeBase+i)
			return
		}
	}
}

// kubeRowFooter is the footer line of the Kubernetes rows.
func (settingsPage *settingsPage) kubeRowFooter(item int) string {
	switch {
	case isKubeClusterItem(item) && settingsPage.kubeDeleteFocused:
		return "  enter: disconnect…  h: back"
	case isKubeClusterItem(item) && settingsPage.kubeTestFocused:
		return "  enter: test now  l: [ delete ]  h: back"
	case isKubeClusterItem(item):
		return "  " + kubeFooter(kubeRowKeys, nil)
	case item == settingsItemKubeAdd:
		return "  enter: pick a context on fleetd's host to connect"
	}
	return ""
}

// openKubePicker opens the context picker: to connect a new cluster, or with
// repoint set to move that connection to another context.
func (settingsPage *settingsPage) openKubePicker(m *model, repoint string) tea.Cmd {
	picker := newKubePicker(m, repoint)
	settingsPage.kubeDialog = picker
	return picker.load(m, nil)
}

// closeKubeDialog closes whatever Kubernetes dialog is open, stopping the
// draft test a picker was running.
func (settingsPage *settingsPage) closeKubeDialog(m *model) {
	if picker, ok := settingsPage.kubeDialog.(*kubePicker); ok {
		picker.stopTest(m)
		settingsPage.kubeDialog = picker.back
		return
	}
	settingsPage.kubeDialog = nil
}

// updateKube routes keys to an open Kubernetes dialog and answers to the
// dialog that asked. handled is false for everything else.
func (settingsPage *settingsPage) updateKube(m *model, msg tea.Msg) (tea.Cmd, bool) {
	if settingsPage.kubeDialog == nil {
		return nil, false
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "ctrl+c", "ctrl+q":
			m.quitting = true
			return tea.Quit, true
		}
		m.message = ""
	}
	return settingsPage.kubeDialog.update(m, settingsPage, msg)
}

// onKubeTestDone reacts to a finished test while Settings is open: the
// picker connects (or explains) its draft; a row's test reports its result.
func (settingsPage *settingsPage) onKubeTestDone(m *model, run *kubeTestRun) tea.Cmd {
	if picker, ok := settingsPage.kubeDialog.(*kubePicker); ok && picker.runKey == run.key {
		return picker.onTestDone(m, run)
	}
	if run.id == "" {
		return nil
	}
	if run.err != nil {
		m.message = "Test failed: " + kubeTestError(run.err)
		return nil
	}
	m.message = fmt.Sprintf("%s %s: %s", kubeGlyphCluster, run.result.GetCluster().DisplayNameOrID(), kubeStateLine(run.result))
	return nil
}

// kubeStateLine is a status in words, for messages.
func kubeStateLine(st *fleetgrpc.KubeClusterStatus) string {
	line := kubeStateWord(st.GetState())
	if msg := st.GetMessage(); msg != "" && msg != line {
		line += " (" + msg + ")"
	}
	return line
}

// viewKubeDialog renders the open Kubernetes dialog in place of the
// Settings list, with the page's message line under it.
func (settingsPage *settingsPage) viewKubeDialog(m *model) string {
	var b strings.Builder
	b.WriteString(renderGradient(nameToBanner("Settings")))
	b.WriteString("\n\n")
	b.WriteString(settingsPage.kubeDialog.view(m, settingsPage))
	b.WriteString("\n")
	if m.message != "" {
		b.WriteString(renderMessage(m.message, m.width))
		b.WriteString("\n")
	}
	return b.String()
}

// kubeDialogFrame frames a dialog's body in the dialog border, as wide as
// the terminal allows up to maxWidth.
func kubeDialogFrame(m *model, body string, maxWidth int) string {
	width := maxWidth
	if m.width > 0 {
		width = min(maxWidth, max(20, m.width-2))
	}
	return framedBox(activeTheme.Accent).Width(width).Render(body)
}

// kubeDialogInner is the text width inside kubeDialogFrame.
func kubeDialogInner(m *model, maxWidth int) int {
	width := maxWidth
	if m.width > 0 {
		width = min(maxWidth, max(20, m.width-2))
	}
	return max(10, width-framedBox(activeTheme.Accent).GetHorizontalFrameSize())
}

// kubeDialogBodyHeight is how many body lines fit on screen under the page
// banner, the frame and the message line (all of them when the height is
// not known, as in tests).
func kubeDialogBodyHeight(m *model, fixed int) int {
	if m.height <= 0 {
		return 1 << 20
	}
	banner := lipgloss.Height(renderGradient(nameToBanner("Settings"))) + 1
	frame := framedBox(activeTheme.Accent).GetVerticalFrameSize()
	return max(3, m.height-banner-frame-fixed-3)
}

// kubeLines appends each line of every string to lines, so a scrolled
// dialog body counts what it draws.
func kubeLines(lines []string, add ...string) []string {
	for _, s := range add {
		lines = append(lines, strings.Split(s, "\n")...)
	}
	return lines
}

// kubeScroll keeps lines [start, end) in a window of height lines starting at
// *offset, and returns the visible slice.
func kubeScroll(lines []string, start, end, height int, offset *int) []string {
	if height >= len(lines) {
		*offset = 0
		return lines
	}
	if start < *offset {
		*offset = start
	}
	if end > *offset+height {
		*offset = end - height
	}
	*offset = max(0, min(*offset, len(lines)-height))
	return lines[*offset : *offset+height]
}
