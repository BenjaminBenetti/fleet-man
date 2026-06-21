package tui

import (
	"slices"
	"strings"
	"testing"

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

	st.triggerType = fleet.TriggerWebhook
	st.filterType = fleet.WebhookFilterRegex
	rows = fp.visibleTriggerRows(m)
	if !slices.Contains(rows, trigRowRegex) || slices.Contains(rows, trigRowCron) || slices.Contains(rows, trigRowJSONPath) {
		t.Fatalf("webhook+regex rows wrong: %v", rows)
	}

	st.filterType = fleet.WebhookFilterJSONPath
	rows = fp.visibleTriggerRows(m)
	if !slices.Contains(rows, trigRowJSONPath) || !slices.Contains(rows, trigRowJSONValue) || slices.Contains(rows, trigRowRegex) {
		t.Fatalf("webhook+jsonpath rows wrong: %v", rows)
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
	fp.toggleTriggerEnabled()
	fp.saveAutomationTrigger(m)
	if m.st.Fleets["alpha"].Settings.Triggers[0].Disabled {
		t.Fatal("toggling Enabled in the dialog should re-enable the trigger")
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
		t.Fatal("clicking the [automations] button should toggle automation mode on")
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
	for _, want := range []string{"[instances]", "triggers", "agents", "nightly", "builder", "+ add trigger", "+ add agent"} {
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
