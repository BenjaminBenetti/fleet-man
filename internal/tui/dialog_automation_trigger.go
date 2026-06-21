package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// dialog_automation_trigger.go is the add/edit-trigger modal (issue #188). A
// trigger fires one or more of the fleet's agents (with a prompt) when its
// condition is met. The fields shown depend on the trigger type (schedule vs
// webhook) and, for webhooks, on the filter type (regex vs json-path) — the
// visible-row list (visibleTriggerRows) recomputes on every change so navigation
// only ever lands on a field that currently applies.

// Trigger dialog field ids. Agent toggle rows use trigRowAgentBase+i so the
// fixed fields and the variable-length agent list never collide.
const (
	trigRowName = iota
	trigRowType
	trigRowEnabled
	trigRowPrompt
	trigRowCron
	trigRowWebhookName
	trigRowFilterType
	trigRowRegex
	trigRowJSONPath
	trigRowJSONValue
	trigRowSave
)

const trigRowAgentBase = 1000

// automationTriggerState holds the add/edit-trigger form (issue #188).
type automationTriggerState struct {
	fleetName   string
	editIdx     int // -1 == creating a new trigger
	row         int
	fieldActive bool
	input       textinput.Model

	name        string
	triggerType fleet.TriggerType
	disabled    bool            // mirrors Trigger.Disabled; shown as an "Enabled" toggle
	agentSel    map[string]bool // agent name -> activated by this trigger
	prompt      string
	cron        string
	webhookName string
	filterType  fleet.WebhookFilterType
	regex       string
	jsonPath    string
	jsonValue   string
	errMsg      string
}

func (fleetPage *fleetPage) openAddTriggerDialog(m *model, fleetName string) tea.Cmd {
	fleetPage.triggerDlg = automationTriggerState{
		fleetName:   fleetName,
		editIdx:     -1,
		input:       fleetPage.triggerDlg.input,
		triggerType: fleet.TriggerSchedule,
		filterType:  fleet.WebhookFilterRegex,
		agentSel:    map[string]bool{},
	}
	// Convenience: pre-select the only agent when there is exactly one.
	if agents := fleetAgents(m, fleetName); len(agents) == 1 {
		fleetPage.triggerDlg.agentSel[agents[0].Name] = true
	}
	fleetPage.mode = viewAutomationTrigger
	return nil
}

func (fleetPage *fleetPage) openEditTriggerDialog(m *model, fleetName string, idx int) tea.Cmd {
	f, ok := m.st.Fleets[fleetName]
	if !ok {
		return nil
	}
	t := triggerAt(f, idx)
	if t == nil {
		return nil
	}
	sel := map[string]bool{}
	for _, name := range t.AgentNames {
		sel[name] = true
	}
	filterType := t.FilterType
	if filterType == "" {
		filterType = fleet.WebhookFilterRegex
	}
	triggerType := t.Type
	if triggerType == "" {
		triggerType = fleet.TriggerSchedule
	}
	fleetPage.triggerDlg = automationTriggerState{
		fleetName:   fleetName,
		editIdx:     idx,
		input:       fleetPage.triggerDlg.input,
		name:        t.Name,
		triggerType: triggerType,
		disabled:    t.Disabled,
		agentSel:    sel,
		prompt:      t.Prompt,
		cron:        t.Cron,
		webhookName: t.WebhookName,
		filterType:  filterType,
		regex:       t.Regex,
		jsonPath:    t.JSONPath,
		jsonValue:   t.JSONValue,
	}
	fleetPage.mode = viewAutomationTrigger
	return nil
}

// visibleTriggerRows is the ordered list of currently-applicable field ids.
func (fleetPage *fleetPage) visibleTriggerRows(m *model) []int {
	st := &fleetPage.triggerDlg
	rows := []int{trigRowName, trigRowType, trigRowEnabled}
	for i := range fleetAgents(m, st.fleetName) {
		rows = append(rows, trigRowAgentBase+i)
	}
	rows = append(rows, trigRowPrompt)
	if st.triggerType == fleet.TriggerWebhook {
		rows = append(rows, trigRowWebhookName, trigRowFilterType)
		if st.filterType == fleet.WebhookFilterJSONPath {
			rows = append(rows, trigRowJSONPath, trigRowJSONValue)
		} else {
			rows = append(rows, trigRowRegex)
		}
	} else {
		rows = append(rows, trigRowCron)
	}
	return append(rows, trigRowSave)
}

func (fleetPage *fleetPage) moveTriggerRow(m *model, delta int) {
	st := &fleetPage.triggerDlg
	rows := fleetPage.visibleTriggerRows(m)
	idx := 0
	for i, r := range rows {
		if r == st.row {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(rows)) % len(rows)
	st.row = rows[idx]
}

func (fleetPage *fleetPage) updateAutomationTrigger(m *model, msg tea.Msg) tea.Cmd {
	st := &fleetPage.triggerDlg
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}

	if st.fieldActive {
		switch key.String() {
		case "enter":
			fleetPage.commitTriggerField()
			return nil
		case "esc":
			st.fieldActive = false
			st.input.Blur()
			return nil
		case "ctrl+c":
			return fleetPage.cancelAutomationTrigger(m)
		}
		var cmd tea.Cmd
		st.input, cmd = st.input.Update(msg)
		return cmd
	}

	switch key.String() {
	case "q", "esc", "ctrl+c":
		return fleetPage.cancelAutomationTrigger(m)
	case "up", "k", "shift+tab":
		fleetPage.moveTriggerRow(m, -1)
		return nil
	case "down", "j", "tab":
		fleetPage.moveTriggerRow(m, 1)
		return nil
	case "enter":
		return fleetPage.triggerRowEnter(m)
	case " ":
		return fleetPage.triggerRowToggle(m)
	case "left", "h", "right", "l":
		fleetPage.triggerRowCycle()
		return nil
	}

	if isDialogTextKey(key) && isTriggerTextRow(st.row) {
		blink := fleetPage.activateTriggerField()
		var cmd tea.Cmd
		st.input, cmd = st.input.Update(msg)
		return tea.Batch(blink, cmd)
	}
	return nil
}

func isTriggerTextRow(row int) bool {
	switch row {
	case trigRowName, trigRowPrompt, trigRowCron, trigRowWebhookName, trigRowRegex, trigRowJSONPath, trigRowJSONValue:
		return true
	}
	return false
}

// triggerRowEnter performs the row's primary action: edit a text field, cycle a
// selector, toggle an agent, or save.
func (fleetPage *fleetPage) triggerRowEnter(m *model) tea.Cmd {
	st := &fleetPage.triggerDlg
	switch {
	case isTriggerTextRow(st.row):
		return fleetPage.activateTriggerField()
	case st.row == trigRowType:
		fleetPage.cycleTriggerType()
	case st.row == trigRowEnabled:
		fleetPage.toggleTriggerEnabled()
	case st.row == trigRowFilterType:
		fleetPage.cycleFilterType()
	case st.row >= trigRowAgentBase:
		fleetPage.toggleTriggerAgent(m)
	case st.row == trigRowSave:
		return fleetPage.saveAutomationTrigger(m)
	}
	return nil
}

// triggerRowToggle is space: toggle an agent or cycle a selector; otherwise it
// falls back to the row's enter action (so space activates text fields too).
func (fleetPage *fleetPage) triggerRowToggle(m *model) tea.Cmd {
	st := &fleetPage.triggerDlg
	switch {
	case st.row >= trigRowAgentBase:
		fleetPage.toggleTriggerAgent(m)
		return nil
	case st.row == trigRowType:
		fleetPage.cycleTriggerType()
		return nil
	case st.row == trigRowEnabled:
		fleetPage.toggleTriggerEnabled()
		return nil
	case st.row == trigRowFilterType:
		fleetPage.cycleFilterType()
		return nil
	}
	return fleetPage.triggerRowEnter(m)
}

// triggerRowCycle handles h/l on a selector row. Both selectors are two-valued,
// so direction is immaterial — left and right just flip to the other option.
func (fleetPage *fleetPage) triggerRowCycle() {
	st := &fleetPage.triggerDlg
	switch st.row {
	case trigRowType:
		fleetPage.cycleTriggerType()
	case trigRowEnabled:
		fleetPage.toggleTriggerEnabled()
	case trigRowFilterType:
		fleetPage.cycleFilterType()
	}
}

func (fleetPage *fleetPage) toggleTriggerEnabled() {
	st := &fleetPage.triggerDlg
	st.disabled = !st.disabled
}

func (fleetPage *fleetPage) cycleTriggerType() {
	st := &fleetPage.triggerDlg
	if st.triggerType == fleet.TriggerSchedule {
		st.triggerType = fleet.TriggerWebhook
	} else {
		st.triggerType = fleet.TriggerSchedule
	}
	// The applicable rows changed; park the cursor back on the type selector.
	st.row = trigRowType
}

func (fleetPage *fleetPage) cycleFilterType() {
	st := &fleetPage.triggerDlg
	if st.filterType == fleet.WebhookFilterRegex {
		st.filterType = fleet.WebhookFilterJSONPath
	} else {
		st.filterType = fleet.WebhookFilterRegex
	}
	st.row = trigRowFilterType
}

func (fleetPage *fleetPage) toggleTriggerAgent(m *model) {
	st := &fleetPage.triggerDlg
	agents := fleetAgents(m, st.fleetName)
	idx := st.row - trigRowAgentBase
	if idx < 0 || idx >= len(agents) {
		return
	}
	name := agents[idx].Name
	if st.agentSel == nil {
		st.agentSel = map[string]bool{}
	}
	st.agentSel[name] = !st.agentSel[name]
}

func (fleetPage *fleetPage) activateTriggerField() tea.Cmd {
	st := &fleetPage.triggerDlg
	st.fieldActive = true
	st.input.SetValue(fleetPage.triggerFieldValue())
	st.input.CursorEnd()
	st.input.Focus()
	return st.input.Cursor.BlinkCmd()
}

func (fleetPage *fleetPage) triggerFieldValue() string {
	st := &fleetPage.triggerDlg
	switch st.row {
	case trigRowName:
		return st.name
	case trigRowPrompt:
		return st.prompt
	case trigRowCron:
		return st.cron
	case trigRowWebhookName:
		return st.webhookName
	case trigRowRegex:
		return st.regex
	case trigRowJSONPath:
		return st.jsonPath
	case trigRowJSONValue:
		return st.jsonValue
	}
	return ""
}

func (fleetPage *fleetPage) commitTriggerField() {
	st := &fleetPage.triggerDlg
	v := st.input.Value()
	switch st.row {
	case trigRowName:
		st.name = v
	case trigRowPrompt:
		st.prompt = v
	case trigRowCron:
		st.cron = v
	case trigRowWebhookName:
		st.webhookName = v
	case trigRowRegex:
		st.regex = v
	case trigRowJSONPath:
		st.jsonPath = v
	case trigRowJSONValue:
		st.jsonValue = v
	}
	st.fieldActive = false
	st.input.Blur()
}

func (fleetPage *fleetPage) cancelAutomationTrigger(m *model) tea.Cmd {
	fleetPage.triggerDlg.fieldActive = false
	fleetPage.triggerDlg.input.Blur()
	fleetPage.mode = viewNormal
	m.message = "Cancelled"
	return nil
}

func (fleetPage *fleetPage) saveAutomationTrigger(m *model) tea.Cmd {
	st := &fleetPage.triggerDlg
	if st.fieldActive {
		fleetPage.commitTriggerField()
	}

	f, ok := m.st.Fleets[st.fleetName]
	if !ok {
		return fleetPage.cancelAutomationTrigger(m)
	}

	// Collect selected agents in the fleet's agent order (stable output).
	var selected []string
	for _, a := range f.Settings.Agents {
		if st.agentSel[a.Name] {
			selected = append(selected, a.Name)
		}
	}

	candidate := fleet.Trigger{
		Name:        st.name,
		Type:        st.triggerType,
		Disabled:    st.disabled,
		AgentNames:  selected,
		Prompt:      st.prompt,
		Cron:        st.cron,
		WebhookName: st.webhookName,
		FilterType:  st.filterType,
		Regex:       st.regex,
		JSONPath:    st.jsonPath,
		JSONValue:   st.jsonValue,
	}

	agentNames := make(map[string]struct{}, len(f.Settings.Agents))
	for _, a := range f.Settings.Agents {
		agentNames[a.Name] = struct{}{}
	}
	norm, err := fleet.NormalizeTrigger(candidate, agentNames)
	if err != nil {
		st.errMsg = err.Error()
		return nil
	}
	for i, t := range f.Settings.Triggers {
		if i != st.editIdx && t.Name == norm.Name {
			st.errMsg = fmt.Sprintf("trigger %q already exists", norm.Name)
			return nil
		}
	}

	newSettings := f.Settings
	newTriggers := append([]fleet.Trigger(nil), f.Settings.Triggers...)
	if st.editIdx >= 0 && st.editIdx < len(newTriggers) {
		newTriggers[st.editIdx] = norm
	} else {
		newTriggers = append(newTriggers, norm)
	}
	newSettings.Triggers = newTriggers

	if err := fleetPage.persistAutomationSettings(m, st.fleetName, newSettings); err != nil {
		st.errMsg = err.Error()
		return nil
	}
	fleetPage.mode = viewNormal
	m.message = fmt.Sprintf("Saved trigger %q", norm.Name)
	return nil
}

func (fleetPage *fleetPage) renderAutomationTriggerDialog(m *model) string {
	st := &fleetPage.triggerDlg
	agents := fleetAgents(m, st.fleetName)

	var b strings.Builder
	b.WriteString("\n")
	title := "New trigger"
	if st.editIdx >= 0 {
		title = "Edit trigger"
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
	fmt.Fprintf(&body, "%s%s %s\n", marker(trigRowName), dialogLabel.Render("Name:   "), field(trigRowName, st.name, "trigger-name"))
	fmt.Fprintf(&body, "%s%s %s\n", marker(trigRowType), dialogLabel.Render("Type:   "), selectorLabel(triggerTypeLabel(st.triggerType)))
	fmt.Fprintf(&body, "%s%s %s\n", marker(trigRowEnabled), dialogLabel.Render("Enabled:"), selectorLabel(enabledLabel(st.disabled)))

	// Agents multi-select.
	body.WriteString(dialogLabel.Render("Agents: "))
	body.WriteString("\n")
	if len(agents) == 0 {
		body.WriteString("    " + dimStyle.Render("(no agents — add an agent first)") + "\n")
	}
	for i, a := range agents {
		box := "[ ]"
		if st.agentSel[a.Name] {
			box = "[x]"
		}
		fmt.Fprintf(&body, "%s    %s %s\n", marker(trigRowAgentBase+i), box, a.Name)
	}

	fmt.Fprintf(&body, "%s%s %s\n", marker(trigRowPrompt), dialogLabel.Render("Prompt: "), field(trigRowPrompt, st.prompt, "(fed to the agent via ${PROMPT})"))

	if st.triggerType == fleet.TriggerWebhook {
		fmt.Fprintf(&body, "%s%s %s\n", marker(trigRowWebhookName), dialogLabel.Render("Webhook:"), field(trigRowWebhookName, st.webhookName, "name (appended to gateway URL)"))
		fmt.Fprintf(&body, "%s%s %s\n", marker(trigRowFilterType), dialogLabel.Render("Filter: "), selectorLabel(filterTypeLabel(st.filterType)))
		if st.filterType == fleet.WebhookFilterJSONPath {
			fmt.Fprintf(&body, "%s%s %s\n", marker(trigRowJSONPath), dialogLabel.Render("JSON path:"), field(trigRowJSONPath, st.jsonPath, "e.g. $.action (required)"))
			fmt.Fprintf(&body, "%s%s %s\n", marker(trigRowJSONValue), dialogLabel.Render("JSON value:"), field(trigRowJSONValue, st.jsonValue, "e.g. opened"))
		} else {
			fmt.Fprintf(&body, "%s%s %s\n", marker(trigRowRegex), dialogLabel.Render("Regex:  "), field(trigRowRegex, st.regex, "event body must match"))
		}
	} else {
		fmt.Fprintf(&body, "%s%s %s\n", marker(trigRowCron), dialogLabel.Render("Cron:   "), field(trigRowCron, st.cron, "0 9 * * 1-5"))
	}

	fmt.Fprintf(&body, "%s%s\n", marker(trigRowSave), saveButtonLabel(st.row == trigRowSave))

	if st.errMsg != "" {
		fmt.Fprintf(&body, "\n%s\n", errorStyle.Render(st.errMsg))
	}
	body.WriteString("\n")
	body.WriteString(dialogHint.Render(automationTriggerHint(st.fieldActive, st.row)))

	b.WriteString(dialogBox.Render(body.String()))
	b.WriteString("\n")
	return b.String()
}

func selectorLabel(text string) string { return fmt.Sprintf("[ %s ]", text) }

func triggerTypeLabel(t fleet.TriggerType) string {
	if t == fleet.TriggerWebhook {
		return "Webhook"
	}
	return "Schedule"
}

// enabledLabel renders the inverse of Trigger.Disabled — the dialog presents the
// state as "Enabled" because that is how users think about it.
func enabledLabel(disabled bool) string {
	if disabled {
		return "no"
	}
	return "yes"
}

func filterTypeLabel(f fleet.WebhookFilterType) string {
	if f == fleet.WebhookFilterJSONPath {
		return "json-path"
	}
	return "regex"
}

func automationTriggerHint(fieldActive bool, row int) string {
	if fieldActive {
		return "[enter] Done editing  [ctrl+c] Cancel"
	}
	if row >= trigRowAgentBase {
		return "[j/k] Move  [space] Toggle agent  [enter] Edit/Cycle  [q/esc] Cancel"
	}
	return "[j/k] Move  [enter] Edit/Toggle  [h/l] Cycle  [q/esc] Cancel"
}
