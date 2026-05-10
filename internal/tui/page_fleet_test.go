package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/instanceops"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
)

// testTracker returns an ActivityTracker pre-loaded with specific states/tools.
func testTracker(states map[string]agentState, tools map[string]state.AgentTool) *ActivityTracker {
	t := NewActivityTracker()
	for k, v := range states {
		t.states[k] = v
	}
	for k, v := range tools {
		t.tools[k] = v
	}
	return t
}

func TestUpdateNormalStopShortcutStopsRunningInstance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusRunning}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowInstance, fleetName: "alpha", instance: inst}}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}},
			},
		},
		fleetPage: fp,
	}

	calledFleet := ""
	calledInstance := ""
	restore := stubToggleInstance(func(fleetName, instanceName string) (*instanceops.Result, error) {
		calledFleet = fleetName
		calledInstance = instanceName
		return &instanceops.Result{
			FleetName:    fleetName,
			InstanceName: instanceName,
			Status:       fleet.StatusStopped,
			Changed:      true,
		}, nil
	})
	defer restore()

	cmd := fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})

	if inst.Status != fleet.StatusStopping {
		t.Fatalf("status = %q, want %q", inst.Status, fleet.StatusStopping)
	}

	msg := cmd().(operationDoneMsg)
	if calledFleet != "alpha" || calledInstance != "agent-1" {
		t.Fatalf("toggle called with %s/%s, want alpha/agent-1", calledFleet, calledInstance)
	}
	if msg.message != "Stopped alpha/agent-1" {
		t.Fatalf("message = %q, want %q", msg.message, "Stopped alpha/agent-1")
	}
}

func TestUpdateNormalStopShortcutStartsStoppedInstance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusStopped}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowInstance, fleetName: "alpha", instance: inst}}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}},
			},
		},
		fleetPage: fp,
	}

	restore := stubToggleInstance(func(fleetName, instanceName string) (*instanceops.Result, error) {
		return &instanceops.Result{
			FleetName:    fleetName,
			InstanceName: instanceName,
			Status:       fleet.StatusRunning,
			Changed:      true,
		}, nil
	})
	defer restore()

	cmd := fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})

	if inst.Status != fleet.StatusStarting {
		t.Fatalf("status = %q, want %q", inst.Status, fleet.StatusStarting)
	}

	msg := cmd().(operationDoneMsg)
	if msg.message != "Started alpha/agent-1" {
		t.Fatalf("message = %q, want %q", msg.message, "Started alpha/agent-1")
	}
}

func TestUpdateNormalStopShortcutRequiresInstanceRow(t *testing.T) {
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{fleetPage: fp}

	called := false
	restore := stubToggleInstance(func(fleetName, instanceName string) (*instanceops.Result, error) {
		called = true
		return nil, nil
	})
	defer restore()

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})

	if called {
		t.Fatal("toggle should not be called for a fleet header row")
	}
	if m.message != "Select an instance" {
		t.Fatalf("message = %q, want %q", m.message, "Select an instance")
	}
}

func TestUpdateNormalStopShortcutSkipsCreatingInstance(t *testing.T) {
	inst := &fleet.Instance{Name: "agent-1", Status: fleet.StatusCreating}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowInstance, fleetName: "alpha", instance: inst}}
	m := &model{fleetPage: fp}

	called := false
	restore := stubToggleInstance(func(fleetName, instanceName string) (*instanceops.Result, error) {
		called = true
		return nil, nil
	})
	defer restore()

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})

	if called {
		t.Fatal("toggle should not be called for a creating instance")
	}
	if m.message != "Instance alpha/agent-1 is creating" {
		t.Fatalf("message = %q, want %q", m.message, "Instance alpha/agent-1 is creating")
	}
}

func TestViewFleetListShowsBranchItemForInstance(t *testing.T) {
	inst := &fleet.Instance{
		Name:         "agent-1",
		Status:       fleet.StatusRunning,
		WorkspaceDir: "/workspace/alpha/agent-1",
	}
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},
		{kind: rowInstance, fleetName: "alpha", instance: inst},
		{kind: rowSettings},
	}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}},
			},
		},
		sessionStore: NewSessionStore(),
		stats:             map[string]*backend.ContainerStats{},
		fleetPage:         fp,
	}

	prevResolveBranch := resolveWorkspaceBranch
	resolveWorkspaceBranch = func(workspaceDir string) string {
		if workspaceDir == "/workspace/alpha/agent-1" {
			return "feature/status-line"
		}
		return ""
	}
	defer func() { resolveWorkspaceBranch = prevResolveBranch }()

	view := fp.viewFleetList(m)
	if !strings.Contains(view, "feature/status-line") {
		t.Fatalf("view missing branch item:\n%s", view)
	}
}

func TestBuildRowsShowsSavedGroupWithoutLiveSessions(t *testing.T) {
	inst := &fleet.Instance{Name: "alpha", Status: fleet.StatusRunning, ContainerID: "abc"}
	fp := newFleetPage()
	fp.savedGroups["abc123"] = savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123", "alpha~abc123~ff00"},
		PaneCount:    2,
	}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"repo": {Name: "repo", Instances: []*fleet.Instance{inst}},
			},
		},
		sessionStore: func() *SessionStore {
			s := NewSessionStore()
			ref := InstanceRef{Fleet: "repo", Instance: "alpha"}
			s.SetExpanded(ref, true)
			s.SetDiscovery(ref, nil)
			return s
		}(),
		fleetPage: fp,
	}

	fp.buildRows(m)

	found := false
	for _, r := range fp.rows {
		if r.kind == rowSession && r.groupID == "abc123" {
			found = true
			if r.sessionName != "alpha~abc123" {
				t.Fatalf("sessionName = %q, want alpha~abc123", r.sessionName)
			}
			if r.groupSize != 2 {
				t.Fatalf("groupSize = %d, want 2", r.groupSize)
			}
		}
	}
	if !found {
		t.Fatalf("saved group row not found in %#v", fp.rows)
	}
}

func TestPruneSavedGroupsKeepsSavedGroupWhenDiscoveryIsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	fp := newFleetPage()
	fp.savedGroups["abc123"] = savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123"},
		PaneCount:    1,
	}
	m := &model{
		st: &state.State{
			Fleets:       map[string]*fleet.Fleet{},
			GroupLayouts: map[string]state.GroupLayout{"abc123": {GroupID: "abc123", InstanceName: "alpha"}},
		},
		fleetPage: fp,
		sessionStore: func() *SessionStore {
			s := NewSessionStore()
			s.SetDiscovery(InstanceRef{Fleet: "repo", Instance: "alpha"}, nil)
			return s
		}(),
	}

	m.pruneSavedGroupsForInstance(InstanceRef{Fleet: "repo", Instance: "alpha"})

	if _, ok := m.fleetPage.savedGroups["abc123"]; !ok {
		t.Fatal("saved group was pruned even though WSL discovery returned no live sessions")
	}
	if _, ok := m.st.GroupLayouts["abc123"]; !ok {
		t.Fatal("state group layout was pruned even though WSL discovery returned no live sessions")
	}
}

func TestViewFleetListOmitsBranchItemWhenBranchIsUnavailable(t *testing.T) {
	inst := &fleet.Instance{
		Name:         "agent-1",
		Status:       fleet.StatusRunning,
		WorkspaceDir: "/workspace/alpha/agent-1",
	}
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},
		{kind: rowInstance, fleetName: "alpha", instance: inst},
		{kind: rowSettings},
	}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}},
			},
		},
		sessionStore: NewSessionStore(),
		stats:             map[string]*backend.ContainerStats{},
		fleetPage:         fp,
	}

	prevResolveBranch := resolveWorkspaceBranch
	resolveWorkspaceBranch = func(string) string { return "" }
	defer func() { resolveWorkspaceBranch = prevResolveBranch }()

	view := fp.viewFleetList(m)
	if strings.Contains(view, "feature/status-line") {
		t.Fatalf("view unexpectedly contains branch item:\n%s", view)
	}
}

func TestViewFleetListShowsAgentWorkingIndicator(t *testing.T) {
	inst := &fleet.Instance{
		Name:         "agent-1",
		Status:       fleet.StatusRunning,
		ContainerID:  "abc123",
		WorkspaceDir: "/workspace/alpha/agent-1",
	}
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},
		{kind: rowInstance, fleetName: "alpha", instance: inst},
		{kind: rowSettings},
	}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}},
			},
		},
		config: &state.Config{
			AgentSettings: state.AgentSettings{ToolSelection: state.AgentToolClaude},
		},
		activity: testTracker(
			map[string]agentState{"abc123": agentWorking},
			map[string]state.AgentTool{"abc123": state.AgentToolClaude},
		),
		sessionStore: NewSessionStore(),
		stats:             map[string]*backend.ContainerStats{},
		fleetPage:         fp,
	}

	prevResolveBranch := resolveWorkspaceBranch
	resolveWorkspaceBranch = func(string) string { return "" }
	defer func() { resolveWorkspaceBranch = prevResolveBranch }()

	view := fp.viewFleetList(m)
	if !strings.Contains(view, "\u25b6") || !strings.Contains(view, "Claude Code") {
		t.Fatalf("view missing working indicator:\n%s", view)
	}
}

func TestViewFleetListShowsAgentWaitingIndicator(t *testing.T) {
	inst := &fleet.Instance{
		Name:         "agent-1",
		Status:       fleet.StatusRunning,
		ContainerID:  "abc123",
		WorkspaceDir: "/workspace/alpha/agent-1",
	}
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},
		{kind: rowInstance, fleetName: "alpha", instance: inst},
		{kind: rowSettings},
	}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}},
			},
		},
		config: &state.Config{
			AgentSettings: state.AgentSettings{ToolSelection: state.AgentToolClaude},
		},
		activity: testTracker(
			map[string]agentState{"abc123": agentWaiting},
			map[string]state.AgentTool{"abc123": state.AgentToolClaude},
		),
		sessionStore: NewSessionStore(),
		stats:             map[string]*backend.ContainerStats{},
		fleetPage:         fp,
	}

	prevResolveBranch := resolveWorkspaceBranch
	resolveWorkspaceBranch = func(string) string { return "" }
	defer func() { resolveWorkspaceBranch = prevResolveBranch }()

	view := fp.viewFleetList(m)
	if !strings.Contains(view, "\u23f8") || !strings.Contains(view, "Claude Code") {
		t.Fatalf("view missing waiting indicator:\n%s", view)
	}
}

func TestViewFleetListShowsAgentOffIndicator(t *testing.T) {
	inst := &fleet.Instance{
		Name:         "agent-1",
		Status:       fleet.StatusRunning,
		ContainerID:  "abc123",
		WorkspaceDir: "/workspace/alpha/agent-1",
	}
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},
		{kind: rowInstance, fleetName: "alpha", instance: inst},
		{kind: rowSettings},
	}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}},
			},
		},
		config: &state.Config{
			AgentSettings: state.AgentSettings{ToolSelection: state.AgentToolClaude},
		},
		activity: testTracker(
			map[string]agentState{"abc123": agentNotRunning},
			nil,
		),
		sessionStore: NewSessionStore(),
		stats:             map[string]*backend.ContainerStats{},
		fleetPage:         fp,
	}

	prevResolveBranch := resolveWorkspaceBranch
	resolveWorkspaceBranch = func(string) string { return "" }
	defer func() { resolveWorkspaceBranch = prevResolveBranch }()

	view := fp.viewFleetList(m)
	if !strings.Contains(view, "idle") {
		t.Fatalf("view missing off/idle indicator:\n%s", view)
	}
	if strings.Contains(view, "\u25b6 Claude Code") || strings.Contains(view, "\u23f8") {
		t.Fatalf("not-running instance should not show working/waiting icon:\n%s", view)
	}
}

func TestViewFleetListNoAgentIndicatorForStoppedInstance(t *testing.T) {
	inst := &fleet.Instance{
		Name:         "agent-1",
		Status:       fleet.StatusStopped,
		ContainerID:  "abc123",
		WorkspaceDir: "/workspace/alpha/agent-1",
	}
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},
		{kind: rowInstance, fleetName: "alpha", instance: inst},
		{kind: rowSettings},
	}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}},
			},
		},
		config: &state.Config{
			AgentSettings: state.AgentSettings{ToolSelection: state.AgentToolClaude},
		},
		activity:          NewActivityTracker(),
		sessionStore: NewSessionStore(),
		stats:             map[string]*backend.ContainerStats{},
		fleetPage:         fp,
	}

	prevResolveBranch := resolveWorkspaceBranch
	resolveWorkspaceBranch = func(string) string { return "" }
	defer func() { resolveWorkspaceBranch = prevResolveBranch }()

	view := fp.viewFleetList(m)
	if strings.Contains(view, "Claude Code") || strings.Contains(view, "idle") {
		t.Fatalf("stopped instance should not have agent indicator:\n%s", view)
	}
}

func stubToggleInstance(fn func(fleetName, instanceName string) (*instanceops.Result, error)) func() {
	prev := toggleInstanceStatus
	toggleInstanceStatus = fn
	return func() {
		toggleInstanceStatus = prev
	}
}

func TestEditInstanceRenamesViaDisplayName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	inst := &fleet.Instance{Name: "agent-1", DisplayName: "agent-1", Status: fleet.StatusRunning}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowInstance, fleetName: "alpha", instance: inst}}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}},
			},
		},
		sessionStore: NewSessionStore(),
		stats:             map[string]*backend.ContainerStats{},
		fleetPage:         fp,
	}

	// Press 'e' on the instance row to open the edit dialog.
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})

	if fp.mode != viewAddInstance || !fp.dialogEditing {
		t.Fatalf("mode = %v, editing = %v; want viewAddInstance in edit mode", fp.mode, fp.dialogEditing)
	}
	if fp.dialogRow != addInstanceRowName {
		t.Fatalf("dialogRow = %d, want addInstanceRowName (%d)", fp.dialogRow, addInstanceRowName)
	}
	if got := fp.textInput.Value(); got != "agent-1" {
		t.Fatalf("prefilled input = %q, want %q", got, "agent-1")
	}

	// Type a new name and submit.
	fp.textInput.SetValue("auth-fix")
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyEnter})

	if fp.mode != viewNormal {
		t.Fatalf("mode after enter = %v, want viewNormal", fp.mode)
	}
	if inst.Name != "agent-1" {
		t.Fatalf("Name = %q, want %q (should be immutable)", inst.Name, "agent-1")
	}
	if inst.DisplayName != "auth-fix" {
		t.Fatalf("DisplayName = %q, want %q", inst.DisplayName, "auth-fix")
	}

	prevResolveBranch := resolveWorkspaceBranch
	resolveWorkspaceBranch = func(string) string { return "" }
	defer func() { resolveWorkspaceBranch = prevResolveBranch }()

	view := fp.viewFleetList(m)
	if !strings.Contains(view, "auth-fix") {
		t.Fatalf("rendered list missing new display name:\n%s", view)
	}
}

func TestEditInstanceRejectsEmptyName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	inst := &fleet.Instance{Name: "agent-1", DisplayName: "agent-1", Status: fleet.StatusRunning, Color: "red"}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowInstance, fleetName: "alpha", instance: inst}}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}},
			},
		},
		fleetPage: fp,
	}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	fp.textInput.SetValue("   ")
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyEnter})

	if inst.DisplayName != "agent-1" {
		t.Fatalf("DisplayName = %q, want unchanged %q", inst.DisplayName, "agent-1")
	}
	if m.message != "Name cannot be empty" {
		t.Fatalf("message = %q, want rejection message", m.message)
	}
	if fp.mode != viewAddInstance {
		t.Fatalf("dialog closed prematurely; mode = %v", fp.mode)
	}
}

// TestMoveCursorToInstanceSkipsBetweenInstances verifies that shift-jump
// navigation lands on the next instance row, skipping sessions, headers,
// and settings rows in between.
func TestMoveCursorToInstanceSkipsBetweenInstances(t *testing.T) {
	inst1 := &fleet.Instance{Name: "agent-1"}
	inst2 := &fleet.Instance{Name: "agent-2"}
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},                                 // 0
		{kind: rowInstance, fleetName: "alpha", instance: inst1},                   // 1
		{kind: rowSession, fleetName: "alpha", instance: inst1, sessionName: "s1"}, // 2
		{kind: rowSession, fleetName: "alpha", instance: inst1, sessionName: "s2"}, // 3
		{kind: rowNewSession, fleetName: "alpha", instance: inst1},                 // 4
		{kind: rowInstance, fleetName: "alpha", instance: inst2},                   // 5
		{kind: rowSettings}, // 6
	}

	// Forward: from instance 1 should jump to instance 2.
	fp.cursor = 1
	fp.moveCursorToInstance(1)
	if fp.cursor != 5 {
		t.Fatalf("forward from instance: cursor = %d, want 5", fp.cursor)
	}

	// Forward: from a session row under instance 1 should also jump to instance 2.
	fp.cursor = 3
	fp.moveCursorToInstance(1)
	if fp.cursor != 5 {
		t.Fatalf("forward from session: cursor = %d, want 5", fp.cursor)
	}

	// Backward: from instance 2 should jump to instance 1.
	fp.cursor = 5
	fp.moveCursorToInstance(-1)
	if fp.cursor != 1 {
		t.Fatalf("backward from instance: cursor = %d, want 1", fp.cursor)
	}

	// Backward: from a session row under instance 1 should jump back to instance 1.
	fp.cursor = 3
	fp.moveCursorToInstance(-1)
	if fp.cursor != 1 {
		t.Fatalf("backward from session: cursor = %d, want 1", fp.cursor)
	}
}

// TestMoveCursorToInstanceWraps verifies wrap-around in both directions when
// the cursor would otherwise run past the ends of the row list.
func TestMoveCursorToInstanceWraps(t *testing.T) {
	inst1 := &fleet.Instance{Name: "agent-1"}
	inst2 := &fleet.Instance{Name: "agent-2"}
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},               // 0
		{kind: rowInstance, fleetName: "alpha", instance: inst1}, // 1
		{kind: rowInstance, fleetName: "alpha", instance: inst2}, // 2
		{kind: rowSettings}, // 3
	}

	// Forward from last instance wraps past settings/header to first instance.
	fp.cursor = 2
	fp.moveCursorToInstance(1)
	if fp.cursor != 1 {
		t.Fatalf("forward wrap: cursor = %d, want 1", fp.cursor)
	}

	// Backward from first instance wraps past header/settings to last instance.
	fp.cursor = 1
	fp.moveCursorToInstance(-1)
	if fp.cursor != 2 {
		t.Fatalf("backward wrap: cursor = %d, want 2", fp.cursor)
	}

	// Forward from settings row (after the last instance) wraps to first instance.
	fp.cursor = 3
	fp.moveCursorToInstance(1)
	if fp.cursor != 1 {
		t.Fatalf("forward wrap from settings: cursor = %d, want 1", fp.cursor)
	}

	// Backward from header row (before the first instance) wraps to last instance.
	fp.cursor = 0
	fp.moveCursorToInstance(-1)
	if fp.cursor != 2 {
		t.Fatalf("backward wrap from header: cursor = %d, want 2", fp.cursor)
	}
}

// TestMoveCursorToInstanceNoInstances verifies the cursor is left untouched
// when there are no instance rows to jump to (e.g. all fleets collapsed).
func TestMoveCursorToInstanceNoInstances(t *testing.T) {
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},
		{kind: rowFleetHeader, fleetName: "beta"},
		{kind: rowSettings},
	}

	fp.cursor = 1
	fp.moveCursorToInstance(1)
	if fp.cursor != 1 {
		t.Fatalf("no instances forward: cursor = %d, want 1", fp.cursor)
	}
	fp.moveCursorToInstance(-1)
	if fp.cursor != 1 {
		t.Fatalf("no instances backward: cursor = %d, want 1", fp.cursor)
	}
}

// TestUpdateNormalShiftJumpKeys verifies that capital J/K and shift+arrow
// keys dispatch to moveCursorToInstance via updateNormal.
func TestUpdateNormalShiftJumpKeys(t *testing.T) {
	inst1 := &fleet.Instance{Name: "agent-1"}
	inst2 := &fleet.Instance{Name: "agent-2"}
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},
		{kind: rowInstance, fleetName: "alpha", instance: inst1},
		{kind: rowSession, fleetName: "alpha", instance: inst1, sessionName: "s1"},
		{kind: rowInstance, fleetName: "alpha", instance: inst2},
		{kind: rowSettings},
	}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst1, inst2}},
			},
		},
		fleetPage: fp,
	}

	// Capital J should jump forward to the next instance.
	fp.cursor = 1
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'J'}})
	if fp.cursor != 3 {
		t.Fatalf("J: cursor = %d, want 3", fp.cursor)
	}

	// Capital K should jump backward to the previous instance.
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'K'}})
	if fp.cursor != 1 {
		t.Fatalf("K: cursor = %d, want 1", fp.cursor)
	}

	// shift+down should behave the same as J.
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyShiftDown})
	if fp.cursor != 3 {
		t.Fatalf("shift+down: cursor = %d, want 3", fp.cursor)
	}

	// shift+up should behave the same as K.
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyShiftUp})
	if fp.cursor != 1 {
		t.Fatalf("shift+up: cursor = %d, want 1", fp.cursor)
	}
}
