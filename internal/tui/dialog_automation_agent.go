package tui

import (
	"errors"
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// dialog_automation_agent.go is the add/edit-agent modal (issue #188). An agent
// defines how an automation worker is launched: a command (with ${PROMPT}/
// ${SYS_PROMPT} placeholders), a system prompt, and the deploy target its
// instances are created on (a backend, or a cluster connection). The command
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
	// target is where the agent's instances are created; targets are what
	// the Backend row cycles (agentTargets).
	target  fleet.DeployTarget
	targets []fleet.DeployTargetOption
	errMsg  string
}

// openAddAgentDialog opens the agent editor for a new agent. The command field
// defaults to the most recently defined agent's command (the issue's "remember
// the last value" behavior), falling back to fleet.DefaultAgentCommand; the
// target is the daemon's remembered deploy target, as the New instance dialog
// opens on, and is stored concretely when the agent is saved (a later change
// of the remembered default never moves an agent).
func (fleetPage *fleetPage) openAddAgentDialog(m *model, fleetName string) tea.Cmd {
	command := fleet.DefaultAgentCommand
	if agents := fleetAgents(m, fleetName); len(agents) > 0 {
		command = agents[len(agents)-1].Command
	}
	targets, def := agentTargets(m, fleetName, fleet.DeployTarget{})
	fleetPage.agentDlg = automationAgentState{
		fleetName: fleetName,
		editIdx:   -1,
		input:     fleetPage.agentDlg.input,
		command:   command,
		target:    def,
		targets:   targets,
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
	target := a.DeployTarget()
	targets, _ := agentTargets(m, fleetName, target)
	fleetPage.agentDlg = automationAgentState{
		fleetName:    fleetName,
		editIdx:      idx,
		input:        fleetPage.agentDlg.input,
		name:         a.Name,
		command:      a.Command,
		systemPrompt: a.SystemPrompt,
		target:       target,
		targets:      targets,
	}
	if agentTargetMissing(m, target) {
		fleetPage.agentDlg.errMsg = missingTargetMessage(m, target)
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
			st.cycleTarget(-1)
			fleetPage.autosaveAgent(m)
		}
		return nil
	case "right", "l":
		if st.row == agentRowBackend {
			st.cycleTarget(1)
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
		st.cycleTarget(1)
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
	// agent is actually discarded. If an edit was left in an unsavable state, say
	// so rather than closing silently (the last good state stayed on disk).
	switch {
	case st.editIdx < 0:
		m.message = "Cancelled"
	case fleetPage.agentFormError(m) != nil:
		m.message = "Closed; last change not saved: " + fleetPage.agentFormError(m).Error()
	default:
		m.message = ""
	}
	return nil
}

// agentFormError returns the validation error the current form would hit if saved
// now, without persisting — used on close to tell the user when an instant-save
// edit was left unsavable. nil when it would save.
func (fleetPage *fleetPage) agentFormError(m *model) error {
	st := &fleetPage.agentDlg
	f, ok := m.st.Fleets[st.fleetName]
	if !ok || st.editIdx < 0 || st.editIdx >= len(f.Settings.Agents) {
		return nil
	}
	if agentTargetMissing(m, st.target) {
		return errors.New(missingTargetMessage(m, st.target))
	}
	oldName := f.Settings.Agents[st.editIdx].Name
	_, err := fleet.UpdateAgent(f.Settings, oldName, fleetPage.agentCandidate())
	return err
}

// agentCandidate builds a fleet.Agent from the current form state.
func (fleetPage *fleetPage) agentCandidate() fleet.Agent {
	st := &fleetPage.agentDlg
	return fleet.Agent{
		Name:         st.name,
		Command:      st.command,
		SystemPrompt: st.systemPrompt,
		Backend:      st.target.Backend,
		KubeCluster:  st.target.KubeCluster,
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
	// A target whose connection is gone (a Forget) must be replaced before
	// anything else about the agent saves: every firing would be refused.
	if agentTargetMissing(m, st.target) {
		st.errMsg = missingTargetMessage(m, st.target)
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
	if agentTargetMissing(m, st.target) {
		st.errMsg = missingTargetMessage(m, st.target)
		return nil
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
	backendRow := fmt.Sprintf("[ %s ]", boundTargetLabel(m, st.target))
	if reason := st.targetReason(); reason != "" {
		backendRow += dimStyle.Render("  " + reason)
	}
	fmt.Fprintf(&body, "%s%s %s\n", marker(agentRowBackend), dialogLabel.Render("Backend: "), backendRow)
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

// agentTargets lists the targets the agent dialog's Backend row cycles for a
// fleet, and the one a new agent opens on: every target the daemon lists for
// the fleet (ListDeployTargets, so a template fleet drops Coder and
// Codespaces), available now or not (an agent fires later; the row says why
// one is not), opening on the daemon's remembered default. current, an
// existing agent's target, is kept in the list when the daemon no longer
// has it (a forgotten cluster), so the row can show it and move off it. A
// daemon that cannot answer leaves the targets this TUI knows: every backend
// and the pushed connections, opening on its config's default.
func agentTargets(m *model, fleetName string, current fleet.DeployTarget) ([]fleet.DeployTargetOption, fleet.DeployTarget) {
	template := false
	if f := m.st.Fleets[fleetName]; f != nil {
		template = fleet.IsTemplateRemote(f.Remote)
	}
	var list deployTargetList
	if fetched, err := loadDeployTargets(m, fleetName, template); err == nil && len(fetched.options) > 0 {
		list = fetched
	} else {
		list = knownAgentTargets(m, template)
	}
	var options []fleet.DeployTargetOption
	seen := map[fleet.DeployTarget]bool{}
	for _, option := range list.options {
		if option.Target.IsZero() || seen[option.Target] {
			continue
		}
		seen[option.Target] = true
		options = append(options, option)
	}
	if !current.IsZero() && !seen[current] {
		options = append([]fleet.DeployTargetOption{{Target: current, Reason: missingTargetMessage(m, current)}}, options...)
	}
	def := fleet.DeployTarget{Backend: fleet.BackendDevcontainer}
	for _, option := range options {
		if option.Target == list.def {
			def = list.def
		}
	}
	return options, def
}

// knownAgentTargets is agentTargets' list without the daemon's: every backend
// (but the ones a template fleet cannot use) and each connection this TUI
// was pushed, opening on its config's default target.
func knownAgentTargets(m *model, template bool) deployTargetList {
	var list deployTargetList
	for _, target := range fleet.DeployTargets() {
		if template && !target.CopiesTemplates() {
			continue
		}
		list.options = append(list.options, fleet.DeployTargetOption{Target: target, Available: true})
	}
	for _, st := range m.kubeStatuses() {
		target := fleet.DeployTarget{Backend: fleet.BackendKubernetes, KubeCluster: st.GetCluster().GetId()}
		list.options = append(list.options, fleet.DeployTargetOption{Target: target, Available: true})
	}
	if m.config != nil && m.config.DefaultBackend != "" {
		list.def = fleet.DeployTarget{Backend: fleet.BackendType(m.config.DefaultBackend)}
		if list.def.Backend == fleet.BackendKubernetes {
			list.def.KubeCluster = m.config.DefaultKubeCluster
		}
	}
	return list
}

// cycleTarget moves the Backend row direction places through st.targets.
func (st *automationAgentState) cycleTarget(direction int) {
	targets := make([]fleet.DeployTarget, len(st.targets))
	for i, option := range st.targets {
		targets[i] = option.Target
	}
	st.target = cycleOption(st.target, direction, targets)
}

// targetReason is why the selected target cannot take a create now, as the
// daemon said; "" when it can (or nobody said).
func (st *automationAgentState) targetReason() string {
	for _, option := range st.targets {
		if option.Target == st.target && !option.Available {
			return option.Reason
		}
	}
	return ""
}

// agentTargetMissing reports whether target is a cluster connection the
// daemon no longer has (removed with Forget): an agent there has every
// firing refused, so the dialog asks for another target before it saves.
func agentTargetMissing(m *model, target fleet.DeployTarget) bool {
	return target.KubeCluster != "" && m.kubeClusters != nil && m.kubeStatus(target.KubeCluster) == nil
}

// missingTargetMessage asks for a new target for one whose connection is
// gone.
func missingTargetMessage(m *model, target fleet.DeployTarget) string {
	return deployTargetLabel(m, target) + " is no longer connected: pick another target"
}
