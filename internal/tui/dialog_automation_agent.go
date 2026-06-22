package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// dialog_automation_agent.go is the add/edit-agent modal (issue #188). An agent
// defines how an automation worker is launched: a command (with ${PROMPT}/
// ${SYS_PROMPT} placeholders), a system prompt, and an env backend. The command
// always runs in a tmux session so the user can open it in the TUI and watch.

// agentRow identifies a focusable field in the agent dialog.
const (
	agentRowName = iota
	agentRowCommand
	agentRowSystemPrompt
	agentRowBackend
	agentRowSave
	agentRowCount
)

// automationAgentState holds the add/edit-agent form (issue #188). The target
// fleet + index live here (not in dlg) so the automation dialogs never collide
// with the instance dialogs' shared scratch.
type automationAgentState struct {
	fleetName   string
	editIdx     int // -1 == creating a new agent
	row         int
	fieldActive bool
	input       textinput.Model

	name         string
	command      string
	systemPrompt string
	backend      fleet.BackendType
	errMsg       string
}

// openAddAgentDialog opens the agent editor for a new agent. The command field
// defaults to the most recently defined agent's command (the issue's "remember
// the last value" behavior), falling back to fleet.DefaultAgentCommand; backend
// defaults to the user's default backend.
func (fleetPage *fleetPage) openAddAgentDialog(m *model, fleetName string) tea.Cmd {
	command := fleet.DefaultAgentCommand
	if agents := fleetAgents(m, fleetName); len(agents) > 0 {
		command = agents[len(agents)-1].Command
	}
	backend := fleet.BackendDevcontainer
	if m.config != nil && m.config.DefaultBackend != "" {
		backend = fleet.BackendType(m.config.DefaultBackend)
	}
	fleetPage.agentDlg = automationAgentState{
		fleetName: fleetName,
		editIdx:   -1,
		input:     fleetPage.agentDlg.input,
		command:   command,
		backend:   backend,
	}
	fleetPage.mode = viewAutomationAgent
	return nil
}

// openEditAgentDialog opens the agent editor for an existing agent.
func (fleetPage *fleetPage) openEditAgentDialog(m *model, fleetName string, idx int) tea.Cmd {
	f, ok := m.st.Fleets[fleetName]
	if !ok {
		return nil
	}
	a := agentAt(f, idx)
	if a == nil {
		return nil
	}
	backend := a.Backend
	if backend == "" {
		backend = fleet.BackendDevcontainer
	}
	fleetPage.agentDlg = automationAgentState{
		fleetName:    fleetName,
		editIdx:      idx,
		input:        fleetPage.agentDlg.input,
		name:         a.Name,
		command:      a.Command,
		systemPrompt: a.SystemPrompt,
		backend:      backend,
	}
	fleetPage.mode = viewAutomationAgent
	return nil
}

// rowCount is the number of navigable rows. Editing an existing agent is
// instant-save and omits the Save row, so it stops one short of agentRowCount.
func (st *automationAgentState) rowCount() int {
	if st.editIdx >= 0 {
		return agentRowSave
	}
	return agentRowCount
}

func (fleetPage *fleetPage) updateAutomationAgent(m *model, msg tea.Msg) tea.Cmd {
	st := &fleetPage.agentDlg
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}

	if st.fieldActive {
		switch key.String() {
		case "enter":
			fleetPage.commitAgentField()
			fleetPage.autosaveAgent(m)
			return nil
		case "esc":
			st.fieldActive = false
			st.input.Blur()
			return nil
		case "ctrl+c":
			return fleetPage.cancelAutomationAgent(m)
		}
		var cmd tea.Cmd
		st.input, cmd = st.input.Update(msg)
		return cmd
	}

	switch key.String() {
	case "q", "esc", "ctrl+c":
		return fleetPage.cancelAutomationAgent(m)
	case "up", "k", "shift+tab":
		n := st.rowCount()
		st.row = (st.row - 1 + n) % n
		return nil
	case "down", "j", "tab":
		n := st.rowCount()
		st.row = (st.row + 1) % n
		return nil
	case "enter", " ":
		return fleetPage.agentRowEnter(m)
	case "left", "h":
		if st.row == agentRowBackend {
			st.backend = nextBackendType(st.backend, -1, allBackendTypes)
			fleetPage.autosaveAgent(m)
		}
		return nil
	case "right", "l":
		if st.row == agentRowBackend {
			st.backend = nextBackendType(st.backend, 1, allBackendTypes)
			fleetPage.autosaveAgent(m)
		}
		return nil
	}

	// A printable key activates an inline text field and feeds the key. The
	// editor-backed system prompt is excluded — it opens $EDITOR on enter instead
	// (inline typing makes no sense for many-line text).
	if isDialogTextKey(key) && agentRowIsText(st.row) && st.row != agentRowSystemPrompt {
		blink := fleetPage.activateAgentField()
		var cmd tea.Cmd
		st.input, cmd = st.input.Update(msg)
		return tea.Batch(blink, cmd)
	}
	return nil
}

func agentRowIsText(row int) bool {
	return row == agentRowName || row == agentRowCommand || row == agentRowSystemPrompt
}

func (fleetPage *fleetPage) agentRowEnter(m *model) tea.Cmd {
	st := &fleetPage.agentDlg
	switch st.row {
	case agentRowName, agentRowCommand:
		return fleetPage.activateAgentField()
	case agentRowSystemPrompt:
		// The system prompt is often many lines — edit it in $EDITOR, not inline.
		return editorCmd(editorTargetAgentSysPrompt, "sysprompt", st.systemPrompt)
	case agentRowBackend:
		st.backend = nextBackendType(st.backend, 1, allBackendTypes)
		fleetPage.autosaveAgent(m)
	case agentRowSave:
		return fleetPage.saveAutomationAgent(m)
	}
	return nil
}

func (fleetPage *fleetPage) activateAgentField() tea.Cmd {
	st := &fleetPage.agentDlg
	st.fieldActive = true
	st.input.SetValue(fleetPage.agentFieldValue())
	st.input.CursorEnd()
	st.input.Focus()
	return st.input.Cursor.BlinkCmd()
}

func (fleetPage *fleetPage) agentFieldValue() string {
	st := &fleetPage.agentDlg
	switch st.row {
	case agentRowName:
		return st.name
	case agentRowCommand:
		return st.command
	case agentRowSystemPrompt:
		return st.systemPrompt
	}
	return ""
}

func (fleetPage *fleetPage) commitAgentField() {
	st := &fleetPage.agentDlg
	v := st.input.Value()
	switch st.row {
	case agentRowName:
		st.name = v
	case agentRowCommand:
		st.command = v
	case agentRowSystemPrompt:
		st.systemPrompt = v
	}
	st.fieldActive = false
	st.input.Blur()
}

func (fleetPage *fleetPage) cancelAutomationAgent(m *model) tea.Cmd {
	st := &fleetPage.agentDlg
	st.fieldActive = false
	st.input.Blur()
	fleetPage.mode = viewNormal
	// Editing is instant-save, so closing is just "done" — only a new (unsaved)
	// agent is actually discarded.
	if st.editIdx < 0 {
		m.message = "Cancelled"
	} else {
		m.message = ""
	}
	return nil
}

// agentCandidate builds a fleet.Agent from the current form state.
func (fleetPage *fleetPage) agentCandidate() fleet.Agent {
	st := &fleetPage.agentDlg
	return fleet.Agent{
		Name:         st.name,
		Command:      st.command,
		SystemPrompt: st.systemPrompt,
		Backend:      st.backend,
	}
}

// autosaveAgent persists the form immediately when editing an existing agent
// (instant-save, like the settings page). It is a no-op for a new agent, whose
// explicit Save button owns creation. A validation/RPC failure surfaces inline
// and leaves the last-good persisted state intact (the in-memory revert lives in
// persistAutomationSettings); a success clears the error. A successful rename
// rewrites every trigger that referenced the old name (fleet.UpdateAgent).
func (fleetPage *fleetPage) autosaveAgent(m *model) {
	st := &fleetPage.agentDlg
	if st.editIdx < 0 {
		return
	}
	f, ok := m.st.Fleets[st.fleetName]
	if !ok || st.editIdx >= len(f.Settings.Agents) {
		return
	}
	oldName := f.Settings.Agents[st.editIdx].Name
	newSettings, err := fleet.UpdateAgent(f.Settings, oldName, fleetPage.agentCandidate())
	if err != nil {
		st.errMsg = err.Error()
		return
	}
	if err := fleetPage.persistAutomationSettings(m, st.fleetName, newSettings); err != nil {
		st.errMsg = err.Error()
		return
	}
	st.errMsg = ""
}

func (fleetPage *fleetPage) saveAutomationAgent(m *model) tea.Cmd {
	st := &fleetPage.agentDlg
	if st.fieldActive {
		fleetPage.commitAgentField()
	}
	candidate := fleetPage.agentCandidate()

	f, ok := m.st.Fleets[st.fleetName]
	if !ok {
		return fleetPage.cancelAutomationAgent(m)
	}

	// fleet.AddAgent/UpdateAgent own the shared invariants (normalize, reject a
	// duplicate name, and — on rename — rewrite every trigger that references
	// the old name so the server never sees a dangling reference).
	var newSettings fleet.FleetSettings
	var err error
	if st.editIdx >= 0 && st.editIdx < len(f.Settings.Agents) {
		oldName := f.Settings.Agents[st.editIdx].Name
		newSettings, err = fleet.UpdateAgent(f.Settings, oldName, candidate)
	} else {
		newSettings, err = fleet.AddAgent(f.Settings, candidate)
	}
	if err != nil {
		st.errMsg = err.Error()
		return nil
	}

	if err := fleetPage.persistAutomationSettings(m, st.fleetName, newSettings); err != nil {
		st.errMsg = err.Error()
		return nil
	}
	fleetPage.mode = viewNormal
	m.message = fmt.Sprintf("Saved agent %q", strings.TrimSpace(candidate.Name))
	return nil
}

func (fleetPage *fleetPage) renderAutomationAgentDialog(m *model) string {
	st := &fleetPage.agentDlg
	var b strings.Builder
	b.WriteString("\n")

	title := "New agent"
	if st.editIdx >= 0 {
		title = "Edit agent"
	}

	marker := func(r int) string {
		if st.row == r {
			return cursorStyle.Render("> ")
		}
		return "  "
	}
	field := func(r int, value, placeholder string) string {
		if st.fieldActive && st.row == r {
			return st.input.View()
		}
		if value == "" {
			return dimStyle.Render(placeholder)
		}
		return value
	}

	var body strings.Builder
	fmt.Fprintf(&body, "%s\n\n", dialogTitle.Render(title))
	fmt.Fprintf(&body, "%s%s %s\n", marker(agentRowName), dialogLabel.Render("Name:    "), field(agentRowName, st.name, "agent-name"))
	fmt.Fprintf(&body, "%s%s %s\n", marker(agentRowCommand), dialogLabel.Render("Command: "), field(agentRowCommand, st.command, fleet.DefaultAgentCommand))
	fmt.Fprintf(&body, "%s%s %s\n", marker(agentRowSystemPrompt), dialogLabel.Render("Sys prompt:"), promptFieldPreview(st.systemPrompt, "(optional, injected into ${SYS_PROMPT})"))
	fmt.Fprintf(&body, "%s%s [ %s ]\n", marker(agentRowBackend), dialogLabel.Render("Backend: "), backendTypeLabel(st.backend))
	// Editing instant-saves, so there is no Save row; a new agent keeps it.
	if st.editIdx < 0 {
		fmt.Fprintf(&body, "%s%s\n", marker(agentRowSave), saveButtonLabel(st.row == agentRowSave))
	}

	if st.errMsg != "" {
		fmt.Fprintf(&body, "\n%s\n", errorStyle.Render(st.errMsg))
	}
	body.WriteString("\n")
	body.WriteString(dialogHint.Render(automationHint(st.fieldActive, st.row == agentRowSystemPrompt, st.editIdx >= 0)))

	b.WriteString(dialogBox.Render(body.String()))
	b.WriteString("\n")
	return b.String()
}

// saveButtonLabel renders the dialogs' shared "[ Save ]" action.
func saveButtonLabel(selected bool) string {
	if selected {
		return selectedStyle.Render("[ Save ]")
	}
	return dimStyle.Render("[ Save ]")
}

// automationHint is the footer hint for the agent dialog. onEditorField promotes
// the "$EDITOR" affordance for the long free-text fields. In edit mode every
// change instant-saves, so the close key reads "Close" (nothing to cancel) and an
// active field's enter reads "Save".
func automationHint(fieldActive, onEditorField, isEdit bool) string {
	if fieldActive {
		if isEdit {
			return "[enter] Save  [esc] Discard edit"
		}
		return "[enter] Done editing  [ctrl+c] Cancel"
	}
	closeKey := "[q/esc] Cancel"
	if isEdit {
		closeKey = "[q/esc] Close"
	}
	if onEditorField {
		return "[enter] Edit in $EDITOR  [j/k] Move  " + closeKey
	}
	return "[j/k] Move  [enter] Edit/Toggle  [h/l] Cycle  " + closeKey
}
