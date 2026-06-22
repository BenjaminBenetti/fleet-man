package tui

import (
	"fmt"
	"net/url"
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
	trigRowWebhookURL
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
// Order: Name, Type, Enabled, Prompt, then the type-specific rows (webhook:
// name, filter, regex/json — schedule: cron), then the Agents list, then the
// webhook URL (last, after Agents), then Save.
func (fleetPage *fleetPage) visibleTriggerRows(m *model) []int {
	st := &fleetPage.triggerDlg
	rows := []int{trigRowName, trigRowType, trigRowEnabled, trigRowPrompt}
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
	for i := range fleetAgents(m, st.fleetName) {
		rows = append(rows, trigRowAgentBase+i)
	}
	if st.triggerType == fleet.TriggerWebhook {
		rows = append(rows, trigRowWebhookURL)
	}
	// Editing an existing trigger is instant-save (like the settings page): every
	// change persists immediately, so there is no Save row. A new trigger still
	// needs an explicit Save to create it.
	if st.editIdx < 0 {
		rows = append(rows, trigRowSave)
	}
	return rows
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
			fleetPage.autosaveTrigger(m)
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
		fleetPage.triggerRowCycle(m)
		return nil
	}

	// A printable key activates an inline text field and feeds the key. The
	// editor-backed prompt is excluded — it opens $EDITOR on enter instead.
	if isDialogTextKey(key) && isTriggerTextRow(st.row) && st.row != trigRowPrompt {
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
	case st.row == trigRowPrompt:
		// The prompt is often many lines — edit it in $EDITOR, not inline.
		return editorCmd(editorTargetTriggerPrompt, "prompt", st.prompt)
	case isTriggerTextRow(st.row):
		return fleetPage.activateTriggerField()
	case st.row == trigRowType:
		fleetPage.cycleTriggerType(m)
	case st.row == trigRowEnabled:
		fleetPage.toggleTriggerEnabled(m)
	case st.row == trigRowFilterType:
		fleetPage.cycleFilterType(m)
	case st.row == trigRowWebhookURL:
		return fleetPage.copyTriggerWebhookURL(m)
	case st.row >= trigRowAgentBase:
		fleetPage.toggleTriggerAgent(m)
	case st.row == trigRowSave:
		return fleetPage.saveAutomationTrigger(m)
	}
	return nil
}

// copyTriggerWebhookURL copies this webhook trigger's full gateway URL to the
// clipboard. It needs both a live gateway-assigned base URL (webhook enabled +
// tunnel connected) and a webhook name to form a complete address.
func (fleetPage *fleetPage) copyTriggerWebhookURL(m *model) tea.Cmd {
	url := triggerWebhookURL(m, fleetPage.triggerDlg.webhookName)
	if url == "" {
		if remoteWebhookBaseURL(m) == "" {
			m.message = "No webhook URL yet — enable Webhook in Settings and connect to the gateway"
		} else {
			m.message = "Set a webhook name first"
		}
		return nil
	}
	m.message = "Webhook URL copied to clipboard"
	return copyToClipboardCmd(url)
}

// triggerWebhookURL builds the full webhook URL for a trigger: the gateway-
// assigned base + "/" + the (path-escaped) name. Empty when either piece is
// missing. The name is escaped so a name with spaces/specials yields a valid,
// copy-pasteable URL; fleetd percent-decodes it back to the raw name on delivery.
func triggerWebhookURL(m *model, name string) string {
	base := remoteWebhookBaseURL(m)
	name = strings.TrimSpace(name)
	if base == "" || name == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/" + url.PathEscape(name)
}

// webhookURLDisplay renders the URL row's value: the full copy-pasteable URL once
// both the gateway base and a name exist, or a dim hint explaining what's still
// missing. The base only appears once the user enabled Webhook in Settings AND
// the gateway tunnel connected (it is a server-pushed computed value).
func webhookURLDisplay(m *model, name string) string {
	if remoteWebhookBaseURL(m) == "" {
		return dimStyle.Render("(enable Webhook in Settings; the URL appears once the gateway connects)")
	}
	if strings.TrimSpace(name) == "" {
		return dimStyle.Render("(set a webhook name above)")
	}
	return triggerWebhookURL(m, name) + "  " + dimStyle.Render("press enter to copy")
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
		fleetPage.cycleTriggerType(m)
		return nil
	case st.row == trigRowEnabled:
		fleetPage.toggleTriggerEnabled(m)
		return nil
	case st.row == trigRowFilterType:
		fleetPage.cycleFilterType(m)
		return nil
	}
	return fleetPage.triggerRowEnter(m)
}

// triggerRowCycle handles h/l on a selector row. Both selectors are two-valued,
// so direction is immaterial — left and right just flip to the other option.
func (fleetPage *fleetPage) triggerRowCycle(m *model) {
	st := &fleetPage.triggerDlg
	switch st.row {
	case trigRowType:
		fleetPage.cycleTriggerType(m)
	case trigRowEnabled:
		fleetPage.toggleTriggerEnabled(m)
	case trigRowFilterType:
		fleetPage.cycleFilterType(m)
	}
}

func (fleetPage *fleetPage) toggleTriggerEnabled(m *model) {
	st := &fleetPage.triggerDlg
	st.disabled = !st.disabled
	fleetPage.autosaveTrigger(m)
}

func (fleetPage *fleetPage) cycleTriggerType(m *model) {
	st := &fleetPage.triggerDlg
	if st.triggerType == fleet.TriggerSchedule {
		st.triggerType = fleet.TriggerWebhook
	} else {
		st.triggerType = fleet.TriggerSchedule
	}
	// The applicable rows changed; park the cursor back on the type selector.
	st.row = trigRowType
	fleetPage.autosaveTrigger(m)
}

func (fleetPage *fleetPage) cycleFilterType(m *model) {
	st := &fleetPage.triggerDlg
	if st.filterType == fleet.WebhookFilterRegex {
		st.filterType = fleet.WebhookFilterJSONPath
	} else {
		st.filterType = fleet.WebhookFilterRegex
	}
	st.row = trigRowFilterType
	fleetPage.autosaveTrigger(m)
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
	fleetPage.autosaveTrigger(m)
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
	st := &fleetPage.triggerDlg
	st.fieldActive = false
	st.input.Blur()
	fleetPage.mode = viewNormal
	// Editing is instant-save, so closing is just "done" — only a new (unsaved)
	// trigger is actually discarded.
	if st.editIdx < 0 {
		m.message = "Cancelled"
	} else {
		m.message = ""
	}
	return nil
}

// triggerCandidate builds a fleet.Trigger from the current form state, selecting
// agents in the fleet's stored order so the output is stable.
func (fleetPage *fleetPage) triggerCandidate(f *fleet.Fleet) fleet.Trigger {
	st := &fleetPage.triggerDlg
	var selected []string
	for _, a := range f.Settings.Agents {
		if st.agentSel[a.Name] {
			selected = append(selected, a.Name)
		}
	}
	return fleet.Trigger{
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
}

// autosaveTrigger persists the form immediately when editing an existing trigger
// (instant-save, like the settings page). It is a no-op for a new trigger, whose
// explicit Save button owns creation. A validation/RPC failure surfaces inline
// and leaves the last-good persisted state intact (the in-memory revert lives in
// persistAutomationSettings); a success clears the error.
func (fleetPage *fleetPage) autosaveTrigger(m *model) {
	st := &fleetPage.triggerDlg
	if st.editIdx < 0 {
		return
	}
	f, ok := m.st.Fleets[st.fleetName]
	if !ok || st.editIdx >= len(f.Settings.Triggers) {
		return
	}
	oldName := f.Settings.Triggers[st.editIdx].Name
	newSettings, err := fleet.UpdateTrigger(f.Settings, oldName, fleetPage.triggerCandidate(f))
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

func (fleetPage *fleetPage) saveAutomationTrigger(m *model) tea.Cmd {
	st := &fleetPage.triggerDlg
	if st.fieldActive {
		fleetPage.commitTriggerField()
	}

	f, ok := m.st.Fleets[st.fleetName]
	if !ok {
		return fleetPage.cancelAutomationTrigger(m)
	}

	candidate := fleetPage.triggerCandidate(f)

	// fleet.AddTrigger/UpdateTrigger own the shared invariants (normalize
	// against the fleet's agents and reject a duplicate name).
	var newSettings fleet.FleetSettings
	var err error
	if st.editIdx >= 0 && st.editIdx < len(f.Settings.Triggers) {
		oldName := f.Settings.Triggers[st.editIdx].Name
		newSettings, err = fleet.UpdateTrigger(f.Settings, oldName, candidate)
	} else {
		newSettings, err = fleet.AddTrigger(f.Settings, candidate)
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
	m.message = fmt.Sprintf("Saved trigger %q", strings.TrimSpace(candidate.Name))
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
	fmt.Fprintf(&body, "%s%s %s\n", marker(trigRowPrompt), dialogLabel.Render("Prompt: "), promptFieldPreview(st.prompt, "(fed to the agent via ${PROMPT})"))

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

	// The webhook URL sits last (after Agents) — it is derived/copy-only, not a
	// field the user fills in, so it reads as a footer to the form.
	if st.triggerType == fleet.TriggerWebhook {
		fmt.Fprintf(&body, "%s%s %s\n", marker(trigRowWebhookURL), dialogLabel.Render("URL:    "), webhookURLDisplay(m, st.webhookName))
	}

	// Editing instant-saves, so there is no Save row; a new trigger keeps it.
	if st.editIdx < 0 {
		fmt.Fprintf(&body, "%s%s\n", marker(trigRowSave), saveButtonLabel(st.row == trigRowSave))
	}

	if st.errMsg != "" {
		fmt.Fprintf(&body, "\n%s\n", errorStyle.Render(st.errMsg))
	}
	body.WriteString("\n")
	body.WriteString(dialogHint.Render(automationTriggerHint(st.fieldActive, st.row, st.editIdx >= 0)))

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

// automationTriggerHint is the footer hint for the trigger dialog. In edit mode
// every change instant-saves, so the close key reads "Close" (nothing to cancel)
// and an active field's enter reads "Save".
func automationTriggerHint(fieldActive bool, row int, isEdit bool) string {
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
	if row == trigRowPrompt {
		return "[enter] Edit in $EDITOR  [j/k] Move  " + closeKey
	}
	if row >= trigRowAgentBase {
		return "[j/k] Move  [space] Toggle agent  [enter] Edit/Cycle  " + closeKey
	}
	if row == trigRowWebhookURL {
		return "[j/k] Move  [enter] Copy URL  " + closeKey
	}
	return "[j/k] Move  [enter] Edit/Toggle  [h/l] Cycle  " + closeKey
}
