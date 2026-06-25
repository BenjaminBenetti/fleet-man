package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
)

// newAutomationModel builds a one-fleet model ("alpha", no instances) with rows
// built and the server-persist stubbed so dialog saves stay in-process.
func newAutomationModel(t *testing.T) (*model, *fleetPage) {
	t.Helper()
	orig := setFleetSettingsRemote
	setFleetSettingsRemote = func(string, fleet.FleetSettings) error { return nil }
	t.Cleanup(func() { setFleetSettingsRemote = orig })

	fp := newFleetPage()
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha"},
			},
		},
		sessionStore: NewSessionStore(),
		fleetPage:    fp,
		width:        120,
		height:       50,
	}
	fp.buildRows(m)
	return m, fp
}

func TestAutomationToggleBuildsGroups(t *testing.T) {
	m, fp := newAutomationModel(t)
	fp.toggleAutomationMode(m, "alpha")

	if !fp.automationMode["alpha"] {
		t.Fatal("automation mode should be on for alpha")
	}
	kinds := rowKinds(fp)
	for _, want := range []rowKind{rowAutomationTriggers, rowNewTrigger, rowAutomationAgents, rowNewAgent} {
		if !slices.Contains(kinds, want) {
			t.Fatalf("automation rows missing %v, got %v", want, kinds)
		}
	}
	// Instance-view rows must be gone.
	if slices.Contains(kinds, rowInstance) {
		t.Fatalf("instance rows should be hidden in automation mode: %v", kinds)
	}
	// Toggling back restores the instance view.
	fp.toggleAutomationMode(m, "alpha")
	if fp.automationMode["alpha"] {
		t.Fatal("automation mode should be off after second toggle")
	}
}

func TestAddAgentThenTrigger(t *testing.T) {
	m, fp := newAutomationModel(t)

	// Add an agent via the dialog state + save path.
	fp.openAddAgentDialog(m, "alpha")
	if fp.agentDlg.command == "" {
		t.Fatal("new agent command should default to a non-empty value")
	}
	fp.agentDlg.name = "builder"
	fp.saveAutomationAgent(m)

	agents := m.st.Fleets["alpha"].Settings.Agents
	if len(agents) != 1 || agents[0].Name != "builder" {
		t.Fatalf("agent not saved: %+v", agents)
	}
	if fp.mode != viewNormal {
		t.Fatalf("dialog should close after save, mode=%v", fp.mode)
	}

	// Add a schedule trigger referencing the agent.
	fp.openAddTriggerDialog(m, "alpha")
	st := &fp.triggerDlg
	if !st.agentSel["builder"] {
		t.Fatal("single agent should be pre-selected")
	}
	st.name = "nightly"
	st.cron = "0 0 * * *"
	fp.saveAutomationTrigger(m)

	triggers := m.st.Fleets["alpha"].Settings.Triggers
	if len(triggers) != 1 || triggers[0].Name != "nightly" {
		t.Fatalf("trigger not saved: %+v", triggers)
	}
	if got := triggers[0].AgentNames; len(got) != 1 || got[0] != "builder" {
		t.Fatalf("trigger agent refs = %v, want [builder]", got)
	}
}

func TestSaveTriggerRejectsBadCron(t *testing.T) {
	m, fp := newAutomationModel(t)
	m.st.Fleets["alpha"].Settings.Agents = []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}}

	fp.openAddTriggerDialog(m, "alpha")
	st := &fp.triggerDlg
	st.name = "bad"
	st.agentSel["a"] = true
	st.cron = "not a cron"
	fp.saveAutomationTrigger(m)

	if st.errMsg == "" {
		t.Fatal("expected an error message for an invalid cron")
	}
	if len(m.st.Fleets["alpha"].Settings.Triggers) != 0 {
		t.Fatal("invalid trigger must not be persisted")
	}
	if fp.mode != viewAutomationTrigger {
		t.Fatal("dialog should stay open on validation error")
	}
}

func TestVisibleTriggerRowsByType(t *testing.T) {
	m, fp := newAutomationModel(t)
	m.st.Fleets["alpha"].Settings.Agents = []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}}
	fp.openAddTriggerDialog(m, "alpha")
	st := &fp.triggerDlg

	st.triggerType = fleet.TriggerSchedule
	rows := fp.visibleTriggerRows(m)
	if !slices.Contains(rows, trigRowCron) || slices.Contains(rows, trigRowWebhookName) {
		t.Fatalf("schedule rows wrong: %v", rows)
	}

	// The webhook URL row shows only for webhook triggers.
	if slices.Contains(rows, trigRowWebhookURL) {
		t.Fatalf("schedule trigger should not show the webhook URL row: %v", rows)
	}

	st.triggerType = fleet.TriggerWebhook
	st.filterType = fleet.WebhookFilterRegex
	rows = fp.visibleTriggerRows(m)
	if !slices.Contains(rows, trigRowRegex) || slices.Contains(rows, trigRowCron) || slices.Contains(rows, trigRowJSONPath) {
		t.Fatalf("webhook+regex rows wrong: %v", rows)
	}
	if !slices.Contains(rows, trigRowWebhookURL) {
		t.Fatalf("webhook trigger should show the URL row: %v", rows)
	}

	st.filterType = fleet.WebhookFilterJSONPath
	rows = fp.visibleTriggerRows(m)
	if !slices.Contains(rows, trigRowJSONPath) || !slices.Contains(rows, trigRowJSONValue) || slices.Contains(rows, trigRowRegex) {
		t.Fatalf("webhook+jsonpath rows wrong: %v", rows)
	}

	// Bash triggers show cron + script, and none of the webhook rows.
	st.triggerType = fleet.TriggerBash
	rows = fp.visibleTriggerRows(m)
	if !slices.Contains(rows, trigRowCron) || !slices.Contains(rows, trigRowScript) {
		t.Fatalf("bash trigger should show cron + script rows: %v", rows)
	}
	if slices.Contains(rows, trigRowWebhookName) || slices.Contains(rows, trigRowWebhookURL) || slices.Contains(rows, trigRowRegex) {
		t.Fatalf("bash trigger should not show webhook rows: %v", rows)
	}
}

// TestCycleTriggerTypeThreeWay confirms the Type selector cycles
// Schedule → Webhook → Bash → Schedule (so the bash type is reachable in the UI).
func TestCycleTriggerTypeThreeWay(t *testing.T) {
	m, fp := newAutomationModel(t)
	m.st.Fleets["alpha"].Settings.Agents = []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}}
	fp.openAddTriggerDialog(m, "alpha")
	st := &fp.triggerDlg

	if st.triggerType != fleet.TriggerSchedule {
		t.Fatalf("new trigger should default to schedule, got %q", st.triggerType)
	}
	// Forward (l/right): Schedule → Webhook → Bash → Schedule.
	for i, w := range []fleet.TriggerType{fleet.TriggerWebhook, fleet.TriggerBash, fleet.TriggerSchedule} {
		fp.cycleTriggerType(m, true)
		if st.triggerType != w {
			t.Fatalf("forward cycle %d: type = %q, want %q", i, st.triggerType, w)
		}
	}
	// Backward (h/left): Schedule → Bash → Webhook → Schedule.
	for i, w := range []fleet.TriggerType{fleet.TriggerBash, fleet.TriggerWebhook, fleet.TriggerSchedule} {
		fp.cycleTriggerType(m, false)
		if st.triggerType != w {
			t.Fatalf("backward cycle %d: type = %q, want %q", i, st.triggerType, w)
		}
	}
}

func TestToggleTriggerDisabled(t *testing.T) {
	m, fp := newAutomationModel(t)
	f := m.st.Fleets["alpha"]
	f.Settings.Agents = []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}}
	f.Settings.Triggers = []fleet.Trigger{{Name: "nightly", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "0 0 * * *"}}

	fp.toggleTriggerDisabled(m, "alpha", 0)
	if !m.st.Fleets["alpha"].Settings.Triggers[0].Disabled {
		t.Fatal("trigger should be disabled after the first toggle")
	}
	fp.toggleTriggerDisabled(m, "alpha", 0)
	if m.st.Fleets["alpha"].Settings.Triggers[0].Disabled {
		t.Fatal("trigger should be enabled again after the second toggle")
	}
}

// TestEditTriggerPreservesDisabled guards the footgun that editing a trigger
// would silently re-enable it: the dialog rebuilds the Trigger from scratch, so
// Disabled has to be loaded in and written back.
func TestEditTriggerPreservesDisabled(t *testing.T) {
	m, fp := newAutomationModel(t)
	f := m.st.Fleets["alpha"]
	f.Settings.Agents = []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}}
	f.Settings.Triggers = []fleet.Trigger{{Name: "nightly", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "0 0 * * *", Disabled: true}}

	fp.openEditTriggerDialog(m, "alpha", 0)
	if !fp.triggerDlg.disabled {
		t.Fatal("edit dialog should load Disabled=true")
	}
	// Edit an unrelated field and save — Disabled must survive the round-trip.
	fp.triggerDlg.prompt = "do the thing"
	fp.saveAutomationTrigger(m)
	if got := m.st.Fleets["alpha"].Settings.Triggers[0]; !got.Disabled || got.Prompt != "do the thing" {
		t.Fatalf("edit dropped Disabled or prompt: %+v", got)
	}

	// The dialog's Enabled toggle flips it back on.
	fp.openEditTriggerDialog(m, "alpha", 0)
	fp.triggerDlg.row = trigRowEnabled
	fp.toggleTriggerEnabled(m)
	fp.saveAutomationTrigger(m)
	if m.st.Fleets["alpha"].Settings.Triggers[0].Disabled {
		t.Fatal("toggling Enabled in the dialog should re-enable the trigger")
	}
}

// --- Instant-save: editing an existing agent/trigger persists on every change,
// with no Save row; creating a new one still needs an explicit Save. ---

func seedTrigger(m *model) *fleet.Fleet {
	f := m.st.Fleets["alpha"]
	f.Settings.Agents = []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}}
	f.Settings.Triggers = []fleet.Trigger{{Name: "nightly", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "0 0 * * *"}}
	return f
}

// TestTriggerSaveRowOnlyWhenCreating: the Save row exists for a new trigger and
// is gone when editing (instant-save).
func TestTriggerSaveRowOnlyWhenCreating(t *testing.T) {
	m, fp := newAutomationModel(t)
	seedTrigger(m)

	fp.openAddTriggerDialog(m, "alpha")
	if !slices.Contains(fp.visibleTriggerRows(m), trigRowSave) {
		t.Fatal("a new trigger must offer a Save row")
	}
	fp.openEditTriggerDialog(m, "alpha", 0)
	if slices.Contains(fp.visibleTriggerRows(m), trigRowSave) {
		t.Fatal("editing instant-saves; there must be no Save row")
	}
}

// TestEditTriggerInstantSavesOnToggle: flipping Enabled while editing persists
// immediately, without an explicit save, and leaves the dialog open.
func TestEditTriggerInstantSavesOnToggle(t *testing.T) {
	m, fp := newAutomationModel(t)
	seedTrigger(m)

	fp.openEditTriggerDialog(m, "alpha", 0)
	fp.triggerDlg.row = trigRowEnabled
	fp.updateAutomationTrigger(m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})

	if !m.st.Fleets["alpha"].Settings.Triggers[0].Disabled {
		t.Fatal("toggling Enabled should instant-save Disabled=true")
	}
	if fp.mode != viewAutomationTrigger {
		t.Fatal("dialog should stay open after an instant-save toggle")
	}
}

// TestEditTriggerInstantSavesFieldCommit: committing an inline text edit (enter)
// persists immediately even though there is no Save row.
func TestEditTriggerInstantSavesFieldCommit(t *testing.T) {
	m, fp := newAutomationModel(t)
	seedTrigger(m)

	fp.openEditTriggerDialog(m, "alpha", 0)
	fp.triggerDlg.row = trigRowName
	fp.updateAutomationTrigger(m, tea.KeyMsg{Type: tea.KeyEnter}) // activate field
	fp.triggerDlg.input.SetValue("renamed")
	fp.updateAutomationTrigger(m, tea.KeyMsg{Type: tea.KeyEnter}) // commit → instant-save

	if got := m.st.Fleets["alpha"].Settings.Triggers[0].Name; got != "renamed" {
		t.Fatalf("name not instant-saved: %q", got)
	}
}

// TestEditTriggerAutosaveValidationKeepsLastGood: an edit that fails validation
// (here, an empty name) surfaces an error and never persists, so the last good
// state survives.
func TestEditTriggerAutosaveValidationKeepsLastGood(t *testing.T) {
	m, fp := newAutomationModel(t)
	seedTrigger(m)

	fp.openEditTriggerDialog(m, "alpha", 0)
	fp.triggerDlg.name = ""
	fp.autosaveTrigger(m)

	if fp.triggerDlg.errMsg == "" {
		t.Fatal("clearing the name should surface a validation error")
	}
	if got := m.st.Fleets["alpha"].Settings.Triggers[0].Name; got != "nightly" {
		t.Fatalf("invalid edit must not persist; name = %q, want nightly", got)
	}
}

// TestAgentSaveRowOnlyWhenCreating: the agent dialog shows a Save button (and
// counts it as a navigable row) only when creating; editing instant-saves.
func TestAgentSaveRowOnlyWhenCreating(t *testing.T) {
	m, fp := newAutomationModel(t)
	m.st.Fleets["alpha"].Settings.Agents = []fleet.Agent{{Name: "builder", Command: "echo hi", Backend: fleet.BackendDevcontainer}}

	fp.openAddAgentDialog(m, "alpha")
	if fp.agentDlg.rowCount() != agentRowCount {
		t.Fatalf("new agent rowCount = %d, want %d", fp.agentDlg.rowCount(), agentRowCount)
	}
	if !strings.Contains(fp.renderAutomationAgentDialog(m), "[ Save ]") {
		t.Fatal("a new agent must show the Save button")
	}

	fp.openEditAgentDialog(m, "alpha", 0)
	if fp.agentDlg.rowCount() != agentRowSave {
		t.Fatalf("edit agent rowCount = %d, want %d (Save excluded)", fp.agentDlg.rowCount(), agentRowSave)
	}
	if strings.Contains(fp.renderAutomationAgentDialog(m), "[ Save ]") {
		t.Fatal("editing instant-saves; the Save button must be gone")
	}
}

// TestEditAgentInstantSavesFieldCommit: committing an inline command edit while
// editing an agent persists immediately and keeps the dialog open.
func TestEditAgentInstantSavesFieldCommit(t *testing.T) {
	m, fp := newAutomationModel(t)
	m.st.Fleets["alpha"].Settings.Agents = []fleet.Agent{{Name: "builder", Command: "echo hi", Backend: fleet.BackendDevcontainer}}

	fp.openEditAgentDialog(m, "alpha", 0)
	fp.agentDlg.row = agentRowCommand
	fp.updateAutomationAgent(m, tea.KeyMsg{Type: tea.KeyEnter}) // activate field
	fp.agentDlg.input.SetValue("echo bye")
	fp.updateAutomationAgent(m, tea.KeyMsg{Type: tea.KeyEnter}) // commit → instant-save

	if got := m.st.Fleets["alpha"].Settings.Agents[0].Command; got != "echo bye" {
		t.Fatalf("command not instant-saved: %q", got)
	}
	if fp.mode != viewAutomationAgent {
		t.Fatal("dialog should stay open after an instant-save edit")
	}
}

// TestEditTriggerTypeSwitchDoesNotFlashError: flipping Type while editing makes
// the other type's required fields appear empty; that transient invalid state
// must not flash a validation error, and must not persist over the last good one.
func TestEditTriggerTypeSwitchDoesNotFlashError(t *testing.T) {
	m, fp := newAutomationModel(t)
	seedTrigger(m) // valid schedule trigger "nightly"

	fp.openEditTriggerDialog(m, "alpha", 0)
	fp.triggerDlg.row = trigRowType
	// Schedule → Webhook: webhook name/regex are still empty.
	fp.updateAutomationTrigger(m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})

	if fp.triggerDlg.errMsg != "" {
		t.Fatalf("a structural type switch must not flash an error, got %q", fp.triggerDlg.errMsg)
	}
	if got := m.st.Fleets["alpha"].Settings.Triggers[0].Type; got != fleet.TriggerSchedule {
		t.Fatalf("incomplete webhook must not persist; type = %q, want schedule", got)
	}
	if fp.mode != viewAutomationTrigger {
		t.Fatal("dialog should stay open during a multi-step type switch")
	}

	// Completing the webhook fields then makes it persist.
	fp.triggerDlg.webhookName = "deploy"
	fp.triggerDlg.regex = ".*"
	fp.autosaveTrigger(m)
	if got := m.st.Fleets["alpha"].Settings.Triggers[0]; got.Type != fleet.TriggerWebhook || got.WebhookName != "deploy" {
		t.Fatalf("completed webhook should persist: %+v", got)
	}
}

// TestEditTriggerCloseSignalsUnsavedChange: closing an edit left in an unsavable
// state warns the user rather than discarding silently, and keeps the last good
// state on disk.
func TestEditTriggerCloseSignalsUnsavedChange(t *testing.T) {
	m, fp := newAutomationModel(t)
	seedTrigger(m)

	fp.openEditTriggerDialog(m, "alpha", 0)
	fp.triggerDlg.name = "" // invalid — never persists
	fp.updateAutomationTrigger(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	if !strings.Contains(m.message, "not saved") {
		t.Fatalf("closing with an unsavable edit should warn, got %q", m.message)
	}
	if got := m.st.Fleets["alpha"].Settings.Triggers[0].Name; got != "nightly" {
		t.Fatalf("last good state must remain on disk; name = %q", got)
	}
}

// TestEditTriggerCloseQuietWhenClean: closing a valid edit says nothing (it was
// already instant-saved).
func TestEditTriggerCloseQuietWhenClean(t *testing.T) {
	m, fp := newAutomationModel(t)
	seedTrigger(m)

	fp.openEditTriggerDialog(m, "alpha", 0)
	fp.updateAutomationTrigger(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if m.message != "" {
		t.Fatalf("closing a clean edit should be silent, got %q", m.message)
	}
}

// TestEditAgentCloseSignalsUnsavedChange mirrors the trigger close-signal for the
// agent dialog (which has no structural switches but can still be left invalid).
func TestEditAgentCloseSignalsUnsavedChange(t *testing.T) {
	m, fp := newAutomationModel(t)
	m.st.Fleets["alpha"].Settings.Agents = []fleet.Agent{{Name: "builder", Command: "echo hi", Backend: fleet.BackendDevcontainer}}

	fp.openEditAgentDialog(m, "alpha", 0)
	fp.agentDlg.name = "" // invalid
	fp.updateAutomationAgent(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	if !strings.Contains(m.message, "not saved") {
		t.Fatalf("closing with an unsavable agent edit should warn, got %q", m.message)
	}
	if got := m.st.Fleets["alpha"].Settings.Agents[0].Name; got != "builder" {
		t.Fatalf("last good agent must remain on disk; name = %q", got)
	}
}

// TestTriggerWebhookURL confirms the dialog builds a copy-pasteable webhook URL
// from the gateway-assigned base + the (escaped) name, and returns "" when either
// piece is missing.
func TestTriggerWebhookURL(t *testing.T) {
	m, _ := newAutomationModel(t)

	// No base URL yet (webhook not connected) → empty regardless of name.
	if got := triggerWebhookURL(m, "deploy"); got != "" {
		t.Fatalf("URL should be empty with no gateway base, got %q", got)
	}

	m.remoteMcpStatus = &fleetgrpc.RemoteMcpStatus{
		State:            fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED,
		PublicWebhookUrl: "https://gw.example.com/webhook/abc123",
	}
	if got, want := triggerWebhookURL(m, "deploy"), "https://gw.example.com/webhook/abc123/deploy"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
	// A name with a space is percent-escaped so the URL stays valid.
	if got, want := triggerWebhookURL(m, "my hook"), "https://gw.example.com/webhook/abc123/my%20hook"; got != want {
		t.Fatalf("escaped URL = %q, want %q", got, want)
	}
	// Empty name → empty (the row shows a hint instead).
	if got := triggerWebhookURL(m, "  "); got != "" {
		t.Fatalf("URL should be empty with a blank name, got %q", got)
	}
}

func TestDeleteAgentBlockedWhenReferenced(t *testing.T) {
	m, fp := newAutomationModel(t)
	f := m.st.Fleets["alpha"]
	f.Settings.Agents = []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}}
	f.Settings.Triggers = []fleet.Trigger{{Name: "t", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}}

	fp.deleteAgent(m, "alpha", 0)
	if len(f.Settings.Agents) != 1 {
		t.Fatal("referenced agent should not be deletable")
	}

	// Removing the trigger first frees the agent for deletion.
	fp.deleteTrigger(m, "alpha", 0)
	fp.deleteAgent(m, "alpha", 0)
	if len(f.Settings.Agents) != 0 {
		t.Fatalf("agent should be deletable once unreferenced: %+v", f.Settings.Agents)
	}
}

// cursorToKind parks the cursor on the first row of the given kind.
func cursorToKind(t *testing.T, fp *fleetPage, k rowKind) {
	t.Helper()
	for i, r := range fp.rows {
		if r.kind == k {
			fp.cursor = i
			return
		}
	}
	t.Fatalf("no row of kind %v in %v", k, rowKinds(fp))
}

// automationModelWithItems builds a fleet in automation mode carrying one agent
// ("builder") and one schedule trigger ("nightly") that does NOT reference it,
// so deletes aren't blocked by the reference guard.
func automationModelWithItems(t *testing.T) (*model, *fleetPage) {
	t.Helper()
	m, fp := newAutomationModel(t)
	f := m.st.Fleets["alpha"]
	f.Settings.Agents = []fleet.Agent{{Name: "builder", Backend: fleet.BackendDevcontainer}}
	f.Settings.Triggers = []fleet.Trigger{{Name: "nightly", Type: fleet.TriggerSchedule, Cron: "0 0 * * *"}}
	fp.toggleAutomationMode(m, "alpha")
	return m, fp
}

func TestAddKeyOpensTriggerDialogInTriggersGroup(t *testing.T) {
	m, fp := automationModelWithItems(t)
	for _, k := range []rowKind{rowAutomationTriggers, rowTrigger, rowNewTrigger} {
		cursorToKind(t, fp, k)
		fp.Update(m, key('a'))
		if fp.mode != viewAutomationTrigger {
			t.Fatalf("a on %v should open the add-trigger dialog, mode=%v", k, fp.mode)
		}
		fp.mode = viewNormal
	}
}

func TestAddKeyOpensAgentDialogInAgentsGroup(t *testing.T) {
	m, fp := automationModelWithItems(t)
	for _, k := range []rowKind{rowAutomationAgents, rowAgent, rowNewAgent} {
		cursorToKind(t, fp, k)
		fp.Update(m, key('a'))
		if fp.mode != viewAutomationAgent {
			t.Fatalf("a on %v should open the add-agent dialog, mode=%v", k, fp.mode)
		}
		fp.mode = viewNormal
	}
}

func TestAddKeyOnHeaderInAutomationModeAddsTrigger(t *testing.T) {
	m, fp := automationModelWithItems(t)
	cursorToFleetHeaderHelper(t, fp)
	fp.Update(m, key('a'))
	if fp.mode != viewAutomationTrigger {
		t.Fatalf("a on the header in automation mode should add a trigger, mode=%v", fp.mode)
	}
}

func TestAddKeyOnHeaderInInstanceModeDoesNotAddTrigger(t *testing.T) {
	// In the instance view, 'a' must NOT route into the trigger dialog — it
	// belongs to the add-instance path (whatever that resolves to here).
	m, fp := newAutomationModel(t)
	cursorToFleetHeaderHelper(t, fp)
	fp.Update(m, key('a'))
	if fp.mode == viewAutomationTrigger {
		t.Fatal("a on the header in instance mode must not open the add-trigger dialog")
	}
}

func TestDeleteTriggerAsksToConfirm(t *testing.T) {
	m, fp := automationModelWithItems(t)
	cursorToKind(t, fp, rowTrigger)

	fp.Update(m, key('d'))
	if fp.mode != viewConfirmDeleteAutomation {
		t.Fatalf("d on a trigger should open the confirm dialog, mode=%v", fp.mode)
	}
	if len(m.st.Fleets["alpha"].Settings.Triggers) != 1 {
		t.Fatal("the trigger must not be deleted before the user confirms")
	}
	if fp.autoDel.kind != rowTrigger || fp.autoDel.name != "nightly" {
		t.Fatalf("confirm target wrong: %+v", fp.autoDel)
	}

	fp.Update(m, key('y'))
	if len(m.st.Fleets["alpha"].Settings.Triggers) != 0 {
		t.Fatal("y should delete the trigger")
	}
	if fp.mode != viewNormal {
		t.Fatalf("dialog should close after confirm, mode=%v", fp.mode)
	}
}

func TestDeleteAgentConfirmCancelKeepsIt(t *testing.T) {
	m, fp := automationModelWithItems(t)
	cursorToKind(t, fp, rowAgent)

	fp.Update(m, key('d'))
	if fp.mode != viewConfirmDeleteAutomation || fp.autoDel.kind != rowAgent {
		t.Fatalf("d on an agent should open the confirm dialog for it, mode=%v target=%+v", fp.mode, fp.autoDel)
	}

	fp.Update(m, key('n'))
	if len(m.st.Fleets["alpha"].Settings.Agents) != 1 {
		t.Fatal("n should cancel — the agent must survive")
	}
	if fp.mode != viewNormal {
		t.Fatalf("dialog should close after cancel, mode=%v", fp.mode)
	}
}

func TestDeleteReferencedAgentSkipsConfirm(t *testing.T) {
	// A referenced agent is refused up front: no confirm dialog, no deletion.
	m, fp := newAutomationModel(t)
	f := m.st.Fleets["alpha"]
	f.Settings.Agents = []fleet.Agent{{Name: "a", Backend: fleet.BackendDevcontainer}}
	f.Settings.Triggers = []fleet.Trigger{{Name: "t", Type: fleet.TriggerSchedule, AgentNames: []string{"a"}, Cron: "* * * * *"}}
	fp.toggleAutomationMode(m, "alpha")
	cursorToKind(t, fp, rowAgent)

	fp.Update(m, key('d'))
	if fp.mode != viewNormal {
		t.Fatalf("a referenced agent must not open the confirm dialog, mode=%v", fp.mode)
	}
	if len(f.Settings.Agents) != 1 {
		t.Fatal("a referenced agent must not be deleted")
	}
	if !strings.Contains(m.message, "used by trigger") {
		t.Fatalf("expected a 'used by trigger' message, got %q", m.message)
	}
}

func cursorToFleetHeaderHelper(t *testing.T, fp *fleetPage) {
	t.Helper()
	cursorToKind(t, fp, rowFleetHeader)
}

func TestHeaderToggleButtonMouseClick(t *testing.T) {
	mp, fp := newAutomationModel(t)
	m := *mp
	m.currentPage = fp

	// Render once so the fleet header's toggle-button span + listRowY are recorded.
	fp.viewFleetList(&m)
	if fp.rows[0].kind != rowFleetHeader || fp.rows[0].toggleX1 <= fp.rows[0].toggleX0 {
		t.Fatalf("toggle button span not recorded on the fleet header: %+v", fp.rows[0])
	}

	// A click inside the [automations] button toggles the fleet into automation mode.
	click := tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      fp.rows[0].toggleX0,
		Y:      fp.listRowY,
	}
	next, _ := m.Update(click)
	if !next.(model).fleetPage.automationMode["alpha"] {
		t.Fatal("clicking the ⟳ toggle should switch automation mode on")
	}

	// A click elsewhere on the header (before the button) must NOT toggle — it
	// collapses the fleet like a normal header click.
	mp2, fp2 := newAutomationModel(t)
	m2 := *mp2
	m2.currentPage = fp2
	fp2.viewFleetList(&m2)
	miss := tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      listContentXOffset, // the cursor column, far left of the button
		Y:      fp2.listRowY,
	}
	next2, _ := m2.Update(miss)
	if next2.(model).fleetPage.automationMode["alpha"] {
		t.Fatal("a click outside the button span must not toggle automation mode")
	}
}

func TestAutomationViewRenders(t *testing.T) {
	m, fp := newAutomationModel(t)
	f := m.st.Fleets["alpha"]
	f.Settings.Agents = []fleet.Agent{{Name: "builder", Backend: fleet.BackendDevcontainer}}
	f.Settings.Triggers = []fleet.Trigger{{Name: "nightly", Type: fleet.TriggerSchedule, AgentNames: []string{"builder"}, Cron: "0 0 * * *"}}
	fp.toggleAutomationMode(m, "alpha")

	out := fp.viewFleetList(m)
	for _, want := range []string{"[ " + instancesMark + " ]", "triggers", "agents", "nightly", "builder", "+ add trigger", "+ add agent"} {
		if !strings.Contains(out, want) {
			t.Fatalf("automation view missing %q\n%s", want, out)
		}
	}

	// The trigger and agent dialogs must render without panicking.
	fp.openAddTriggerDialog(m, "alpha")
	if d := fp.renderAutomationTriggerDialog(m); !strings.Contains(d, "New trigger") {
		t.Fatalf("trigger dialog render: %q", d)
	}
	fp.openAddAgentDialog(m, "alpha")
	if d := fp.renderAutomationAgentDialog(m); !strings.Contains(d, "New agent") {
		t.Fatalf("agent dialog render: %q", d)
	}
}

func TestRenameAgentUpdatesTriggerRefs(t *testing.T) {
	m, fp := newAutomationModel(t)
	f := m.st.Fleets["alpha"]
	f.Settings.Agents = []fleet.Agent{{Name: "old", Backend: fleet.BackendDevcontainer}}
	f.Settings.Triggers = []fleet.Trigger{{Name: "t", Type: fleet.TriggerSchedule, AgentNames: []string{"old"}, Cron: "* * * * *"}}

	fp.openEditAgentDialog(m, "alpha", 0)
	fp.agentDlg.name = "renamed"
	fp.saveAutomationAgent(m)

	if f.Settings.Agents[0].Name != "renamed" {
		t.Fatalf("agent not renamed: %+v", f.Settings.Agents)
	}
	if got := f.Settings.Triggers[0].AgentNames; len(got) != 1 || got[0] != "renamed" {
		t.Fatalf("trigger ref not updated on rename: %v", got)
	}
}
