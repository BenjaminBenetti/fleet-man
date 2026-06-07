package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
)

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
	restore := stubToggleInstance(func(fleetName, instanceName string) {
		calledFleet = fleetName
		calledInstance = instanceName
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

	restore := stubToggleInstance(func(fleetName, instanceName string) {})
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
	restore := stubToggleInstance(func(fleetName, instanceName string) {
		called = true
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
	restore := stubToggleInstance(func(fleetName, instanceName string) {
		called = true
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
		fleetPage:    fp,
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
	key := computeGroupKey("alpha", "abc123")
	fp.savedGroups[key] = savedGroup{
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
	key := computeGroupKey("alpha", "abc123")
	fp.savedGroups[key] = savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123"},
		PaneCount:    1,
	}
	m := &model{
		st: &state.State{
			Fleets:       map[string]*fleet.Fleet{},
			GroupLayouts: map[string]state.GroupLayout{key: {GroupID: "abc123", InstanceName: "alpha"}},
		},
		fleetPage: fp,
		sessionStore: func() *SessionStore {
			s := NewSessionStore()
			s.SetDiscovery(InstanceRef{Fleet: "repo", Instance: "alpha"}, nil)
			return s
		}(),
	}

	m.pruneSavedGroupsForInstance(InstanceRef{Fleet: "repo", Instance: "alpha"})

	if _, ok := m.fleetPage.savedGroups[key]; !ok {
		t.Fatal("saved group was pruned even though WSL discovery returned no live sessions")
	}
	if _, ok := m.st.GroupLayouts[key]; !ok {
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
		fleetPage:    fp,
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
		runtime: map[string]*fleetgrpc.InstanceRuntime{
			"alpha/agent-1": {
				Fleet:         "alpha",
				Instance:      "agent-1",
				AgentTool:     fleetgrpc.AgentTool_AGENT_TOOL_CLAUDE,
				AgentActivity: fleetgrpc.AgentActivity_AGENT_ACTIVITY_WORKING,
			},
		},
		sessionStore: NewSessionStore(),
		fleetPage:    fp,
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
		runtime: map[string]*fleetgrpc.InstanceRuntime{
			"alpha/agent-1": {
				Fleet:         "alpha",
				Instance:      "agent-1",
				AgentTool:     fleetgrpc.AgentTool_AGENT_TOOL_CLAUDE,
				AgentActivity: fleetgrpc.AgentActivity_AGENT_ACTIVITY_WAITING,
			},
		},
		sessionStore: NewSessionStore(),
		fleetPage:    fp,
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
		sessionStore: NewSessionStore(),
		fleetPage:    fp,
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
		sessionStore: NewSessionStore(),
		fleetPage:    fp,
	}

	prevResolveBranch := resolveWorkspaceBranch
	resolveWorkspaceBranch = func(string) string { return "" }
	defer func() { resolveWorkspaceBranch = prevResolveBranch }()

	view := fp.viewFleetList(m)
	if strings.Contains(view, "Claude Code") || strings.Contains(view, "idle") {
		t.Fatalf("stopped instance should not have agent indicator:\n%s", view)
	}
}

// stubToggleInstance stubs both lifecycle seams the 's' shortcut dispatches
// (start/stop are separate server jobs now); record fires with the fleet +
// instance whenever either is invoked.
func stubToggleInstance(record func(fleetName, instanceName string)) func() {
	prevStart, prevStop := startInstanceRemote, stopInstanceRemote
	startInstanceRemote = func(fleetName, instanceName string) error {
		record(fleetName, instanceName)
		return nil
	}
	stopInstanceRemote = func(fleetName, instanceName string) error {
		record(fleetName, instanceName)
		return nil
	}
	return func() {
		startInstanceRemote, stopInstanceRemote = prevStart, prevStop
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
		fleetPage:    fp,
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

	// First Enter activates the name field, then type and submit with another Enter.
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !fp.dialogFieldActive {
		t.Fatalf("expected name field to activate after first enter")
	}
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
	// Activate the name field, then submit an empty value.
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyEnter})
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

// stubFleetSettingsSave makes the instant-save RPC a no-op for tests: the dialog
// mutates m.st in-memory before calling it, so a nil return means "saved" with
// no real RPC and no revert.
func stubFleetSettingsSave(t *testing.T) {
	t.Helper()
	orig := setFleetSettingsRemote
	setFleetSettingsRemote = func(string, fleet.FleetSettings) error { return nil }
	t.Cleanup(func() { setFleetSettingsRemote = orig })
}

// TestEditFleetTogglesAndSavesSettings verifies that pressing 'e' on a fleet
// header opens the edit-fleet dialog and that toggling a row's boolean persists
// it IMMEDIATELY (instant-save), with esc just closing the already-saved dialog.
func TestEditFleetTogglesAndSavesSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	f := &fleet.Fleet{Name: "alpha", Instances: []*fleet.Instance{}}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{"alpha": f},
		},
		fleetPage: fp,
	}

	// Press 'e' on the fleet header to open the edit dialog.
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})

	if fp.mode != viewEditFleet {
		t.Fatalf("mode = %v, want viewEditFleet", fp.mode)
	}
	if fp.dialogFleet != "alpha" {
		t.Fatalf("dialogFleet = %q, want %q", fp.dialogFleet, "alpha")
	}
	if fp.dialogClaudeMount || fp.dialogCodexMount {
		t.Fatalf("expected mounts off by default; got claude=%v codex=%v", fp.dialogClaudeMount, fp.dialogCodexMount)
	}

	// Toggle Claude (cursor starts on row 0), move down, toggle Codex — each
	// toggle saves instantly.
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace})

	if !fp.dialogClaudeMount || !fp.dialogCodexMount {
		t.Fatalf("after toggles: claude=%v codex=%v, want both true", fp.dialogClaudeMount, fp.dialogCodexMount)
	}
	// Instant-save: already persisted, no submit needed.
	if !f.Settings.ClaudeCodeMount || !f.Settings.CodexMount {
		t.Fatalf("settings not persisted instantly: %+v", f.Settings)
	}

	// Esc just closes the (already-saved) dialog.
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEsc})
	if fp.mode != viewNormal {
		t.Fatalf("mode after esc = %v, want viewNormal", fp.mode)
	}
}

// TestEditFleetSavesPreferFleetLaunch verifies the "Prefer Fleet Launch"
// checkbox toggles and persists to FleetSettings instantly.
func TestEditFleetSavesPreferFleetLaunch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	f := &fleet.Fleet{Name: "alpha"}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}},
		fleetPage: fp,
	}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if fp.dialogPreferFleetLaunch {
		t.Fatalf("expected PreferFleetLaunch off by default")
	}

	// Navigate to the Prefer Fleet Launch row (last row) and toggle it — this
	// saves instantly.
	for fp.dialogRow != editFleetRowPreferFleetLaunch {
		fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace})
	if !fp.dialogPreferFleetLaunch {
		t.Fatalf("toggle did not set dialogPreferFleetLaunch")
	}

	if !f.Settings.PreferFleetLaunchEnabled() {
		t.Fatalf("Settings.PreferFleetLaunch = %v, want enabled (instant-save)", f.Settings.PreferFleetLaunch)
	}
}

// navigateToBuildkitRow opens (assumes already open) the dialog's Caching
// section and lands the cursor on the Buildkit row.
func navigateToBuildkitRow(t *testing.T, fp *fleetPage, m *model) {
	t.Helper()
	guard := 0
	for fp.dialogRow != editFleetRowCaching {
		fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
		if guard++; guard > 20 {
			t.Fatal("never reached Caching header")
		}
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}) // expand
	if !fp.dialogCachingExpanded {
		t.Fatal("Caching section should expand on l")
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown}) // into Buildkit row
	if fp.dialogRow != editFleetRowBuildkit {
		t.Fatalf("dialogRow = %d, want buildkit row", fp.dialogRow)
	}
}

// TestEditFleetSavesBuildkitServer verifies the "Buildkit server" checkbox
// (inside the Caching section) toggles and persists instantly, and that
// toggling it does NOT kick off home-dir detection (it is not a mount).
func TestEditFleetSavesBuildkitServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	f := &fleet.Fleet{Name: "alpha"}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}},
		fleetPage: fp,
	}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if fp.dialogBuildkitServer {
		t.Fatalf("expected BuildkitServer off by default")
	}

	navigateToBuildkitRow(t, fp, m)
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace})
	if !fp.dialogBuildkitServer {
		t.Fatalf("toggle did not set dialogBuildkitServer")
	}
	// Buildkit is not a home-dir mount, so toggling it must not start detection.
	if fp.dialogDetecting {
		t.Fatalf("buildkit toggle should not kick off home-dir detection")
	}
	// Instant-save: persisted on toggle, no submit step.
	if !f.Settings.BuildkitServer {
		t.Fatalf("Settings.BuildkitServer = %v, want true (instant-save)", f.Settings.BuildkitServer)
	}
}

// TestEditFleetCachingExpandCollapse verifies the Caching section starts
// collapsed (hiding the Buildkit row) and expands/collapses with l/h.
func TestEditFleetCachingExpandCollapse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)
	f := &fleet.Fleet{Name: "alpha"}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if fp.dialogCachingExpanded {
		t.Fatal("Caching should start collapsed")
	}
	if slices.Contains(fp.visibleEditFleetRows(), editFleetRowBuildkit) {
		t.Fatal("Buildkit row should be hidden while Caching is collapsed")
	}
	for fp.dialogRow != editFleetRowCaching {
		fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if !fp.dialogCachingExpanded || !slices.Contains(fp.visibleEditFleetRows(), editFleetRowBuildkit) {
		t.Fatal("l should expand Caching and reveal the Buildkit row")
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	if fp.dialogCachingExpanded {
		t.Fatal("h should collapse Caching")
	}
}

// TestEditFleetDeleteCacheButtonHiddenWhenOff verifies the Delete-cache button
// is not reachable while Buildkit is disabled.
func TestEditFleetDeleteCacheButtonHiddenWhenOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)
	f := &fleet.Fleet{Name: "alpha"}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	navigateToBuildkitRow(t, fp, m) // buildkit is OFF
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if fp.dialogCacheButtonFocused {
		t.Fatal("l must not focus the delete-cache button when Buildkit is off")
	}
}

// TestEditFleetDeleteCacheButtonFlow drives the full button interaction:
// focus → arm confirm → wipe (async) → done.
func TestEditFleetDeleteCacheButtonFlow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	var deleted string
	origDel := deleteBuildkitCacheRemote
	deleteBuildkitCacheRemote = func(fleetName string) error { deleted = fleetName; return nil }
	t.Cleanup(func() { deleteBuildkitCacheRemote = origDel })

	f := &fleet.Fleet{Name: "alpha"}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	navigateToBuildkitRow(t, fp, m)
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace}) // enable buildkit → button appears
	if !fp.dialogBuildkitServer {
		t.Fatal("buildkit not enabled")
	}

	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}) // focus button
	if !fp.dialogCacheButtonFocused {
		t.Fatal("l should focus the delete-cache button")
	}

	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter}) // arm inline confirm
	if !fp.dialogDeleteCacheConfirm || fp.dialogDeleting {
		t.Fatalf("first enter should only arm confirm; confirm=%v deleting=%v", fp.dialogDeleteCacheConfirm, fp.dialogDeleting)
	}

	cmd := fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter}) // confirm → wipe
	if fp.dialogDeleteCacheConfirm {
		t.Fatal("confirm should clear once the wipe starts")
	}
	if !fp.dialogDeleting {
		t.Fatal("deletingCache should be set while the wipe runs")
	}
	if cmd == nil {
		t.Fatal("expected a delete-cache command")
	}
	done, ok := cmd().(deleteCacheDoneMsg)
	if !ok {
		t.Fatal("delete cmd did not return deleteCacheDoneMsg")
	}
	if deleted != "alpha" {
		t.Fatalf("delete RPC called for %q, want alpha", deleted)
	}
	fp.handleDeleteCacheDone(m, done)
	if fp.dialogDeleting {
		t.Fatal("deletingCache should clear after the wipe completes")
	}
}

// TestEditFleetDeleteCacheError surfaces the failure path: a failed wipe clears
// the in-flight flag and reports the error to the user.
func TestEditFleetDeleteCacheError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fp := newFleetPage()
	fp.dialogFleet = "alpha"
	fp.dialogDeleting = true
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha"}}}, fleetPage: fp}

	fp.handleDeleteCacheDone(m, deleteCacheDoneMsg{fleet: "alpha", err: errors.New("docker daemon unavailable")})

	if fp.dialogDeleting {
		t.Fatal("deletingCache should clear even on error")
	}
	if !strings.Contains(m.message, "docker daemon unavailable") {
		t.Fatalf("message = %q, want it to surface the error", m.message)
	}
}

// TestEditFleetDeleteCacheConfirmEscCancels verifies esc cancels an armed
// confirm without closing the dialog or wiping.
func TestEditFleetDeleteCacheConfirmEscCancels(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)
	f := &fleet.Fleet{Name: "alpha"}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	navigateToBuildkitRow(t, fp, m)
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace})                     // enable
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}) // focus button
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter})                     // arm confirm

	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEsc}) // cancel confirm
	if fp.dialogDeleteCacheConfirm {
		t.Fatal("esc should cancel the armed confirm")
	}
	if fp.mode != viewEditFleet {
		t.Fatal("esc on an armed confirm should NOT close the dialog")
	}
}

// TestEditFleetHomedirDetectedFillsEmptyInput verifies the success
// path of auto-detection: when the result arrives and the user has
// not typed anything, the home-dir input is populated.
func TestEditFleetHomedirDetectedFillsEmptyInput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	f := &fleet.Fleet{Name: "alpha", Remote: "git@example.com:org/repo.git"}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}},
		fleetPage: fp,
	}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace}) // toggle Claude → kicks detect
	if !fp.dialogDetecting {
		t.Fatalf("expected dialogDetecting to be true after toggle-on")
	}

	fp.handleHomedirDetected(m, homedirDetectedMsg{fleetName: "alpha", homeDir: "/home/node"})

	if fp.dialogDetecting {
		t.Fatalf("dialogDetecting still true after result arrived")
	}
	if got := fp.homedirInput.Value(); got != "/home/node" {
		t.Fatalf("homedirInput = %q, want %q", got, "/home/node")
	}
}

// TestEditFleetHomedirDetectedRespectsUserInput verifies that an
// auto-detected value is discarded when the user has already typed
// something into the home-dir field.
func TestEditFleetHomedirDetectedRespectsUserInput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	f := &fleet.Fleet{Name: "alpha", Remote: "git@example.com:org/repo.git"}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}},
		fleetPage: fp,
	}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace}) // toggle Claude → kicks detect
	fp.homedirInput.SetValue("/custom/path")              // simulate user typing while detect runs

	fp.handleHomedirDetected(m, homedirDetectedMsg{fleetName: "alpha", homeDir: "/home/node"})

	if got := fp.homedirInput.Value(); got != "/custom/path" {
		t.Fatalf("homedirInput = %q, want unchanged %q", got, "/custom/path")
	}
}

// TestEditFleetHomedirDetectedIgnoresStaleFleet verifies that a
// detection result for a fleet other than the one currently being
// edited is dropped, so closing one dialog and opening another never
// has cross-talk.
func TestEditFleetHomedirDetectedIgnoresStaleFleet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	f := &fleet.Fleet{Name: "alpha", Remote: "git@example.com:org/repo.git"}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}},
		fleetPage: fp,
	}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace})

	// Result for a different fleet — must be ignored, and must not
	// clear our in-flight flag.
	fp.handleHomedirDetected(m, homedirDetectedMsg{fleetName: "beta", homeDir: "/wrong"})

	if got := fp.homedirInput.Value(); got != "" {
		t.Fatalf("homedirInput = %q, want empty", got)
	}
	if !fp.dialogDetecting {
		t.Fatalf("dialogDetecting cleared by stale-fleet result; should remain true")
	}
}

// TestEditFleetSavesHomedir verifies the home-dir text field is persisted to
// FleetSettings when the user commits the edit (Enter within the field), under
// the instant-save model.
func TestEditFleetSavesHomedir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	f := &fleet.Fleet{Name: "alpha", Remote: "git@example.com:org/repo.git"}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}},
		fleetPage: fp,
	}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	// Navigate to the Home dir row and open the field.
	for fp.dialogRow != editFleetRowHomeDir {
		fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter}) // activate field
	if !fp.dialogFieldActive {
		t.Fatal("expected home-dir field active after enter")
	}
	fp.homedirInput.SetValue("/opt/agent")
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter}) // commit → instant-save

	if fp.dialogFieldActive {
		t.Fatal("field should be inactive after commit")
	}
	if f.Settings.HomeDir != "/opt/agent" {
		t.Fatalf("Settings.HomeDir = %q, want %q", f.Settings.HomeDir, "/opt/agent")
	}
}

// TestEditFleetEscKeepsInstantSaves verifies the instant-save contract: a toggle
// persists immediately, and esc just closes the dialog WITHOUT discarding it.
func TestEditFleetEscKeepsInstantSaves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	f := &fleet.Fleet{Name: "alpha", Instances: []*fleet.Instance{}}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{
		st: &state.State{
			Fleets: map[string]*fleet.Fleet{"alpha": f},
		},
		fleetPage: fp,
	}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace}) // toggle claude on → saved now
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEsc})   // just closes

	if fp.mode != viewNormal {
		t.Fatalf("mode after esc = %v, want viewNormal", fp.mode)
	}
	if !f.Settings.ClaudeCodeMount {
		t.Fatalf("ClaudeCodeMount = false; instant-save toggle must persist through esc")
	}
}

// TestEditFleetPreservesPFLNilOnUnrelatedEdit verifies instant-save does NOT
// collapse a "never asked" (nil) PreferFleetLaunch into an explicit value when
// the user edits an unrelated setting.
func TestEditFleetPreservesPFLNilOnUnrelatedEdit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	f := &fleet.Fleet{Name: "alpha"} // PreferFleetLaunch nil (never asked)
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace}) // toggle Claude (unrelated)

	if !f.Settings.ClaudeCodeMount {
		t.Fatal("claude mount not persisted")
	}
	if f.Settings.PreferFleetLaunch != nil {
		t.Fatalf("PreferFleetLaunch collapsed to %v on unrelated edit; want nil", *f.Settings.PreferFleetLaunch)
	}
}

// TestEditFleetPFLSetFlagRevertedOnSaveFailure guards the tri-state invariant
// across a FAILED PreferFleetLaunch toggle: the set-flag must revert so a later
// unrelated (successful) save does not collapse the nil tri-state.
func TestEditFleetPFLSetFlagRevertedOnSaveFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	failNext := false
	orig := setFleetSettingsRemote
	setFleetSettingsRemote = func(string, fleet.FleetSettings) error {
		if failNext {
			return errors.New("simulated save failure")
		}
		return nil
	}
	t.Cleanup(func() { setFleetSettingsRemote = orig })

	f := &fleet.Fleet{Name: "alpha"} // PreferFleetLaunch nil
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	for fp.dialogRow != editFleetRowPreferFleetLaunch {
		fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
	}

	// Toggle PFL with a save that fails — must revert both the value and the flag.
	failNext = true
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace})
	failNext = false
	if fp.dialogPreferFleetLaunch {
		t.Fatal("dialogPreferFleetLaunch should revert after a failed save")
	}
	if fp.dialogPreferFleetLaunchSet {
		t.Fatal("dialogPreferFleetLaunchSet should revert to false after a failed save")
	}

	// An unrelated, successful edit must still leave the nil tri-state intact.
	fp.dialogRow = editFleetRowClaude
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace})
	if !f.Settings.ClaudeCodeMount {
		t.Fatal("claude mount not persisted")
	}
	if f.Settings.PreferFleetLaunch != nil {
		t.Fatalf("PreferFleetLaunch collapsed to %v after a failed PFL toggle; want nil", *f.Settings.PreferFleetLaunch)
	}
}

// TestEditFleetHomedirEscDiscards verifies that esc inside the home-dir field
// discards the uncommitted edit and restores the saved value (instant-save only
// commits on Enter).
func TestEditFleetHomedirEscDiscards(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	f := &fleet.Fleet{Name: "alpha", Settings: fleet.FleetSettings{HomeDir: "/home/node"}}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	for fp.dialogRow != editFleetRowHomeDir {
		fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter}) // activate field
	fp.homedirInput.SetValue("/opt/agent")                // uncommitted edit
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEsc})   // discard

	if fp.dialogFieldActive {
		t.Fatal("field should be inactive after esc")
	}
	if got := fp.homedirInput.Value(); got != "/home/node" {
		t.Fatalf("home-dir input = %q after esc-discard, want restored %q", got, "/home/node")
	}
	if f.Settings.HomeDir != "/home/node" {
		t.Fatalf("Settings.HomeDir = %q, want unchanged %q", f.Settings.HomeDir, "/home/node")
	}
}

// TestAddFleetInspectedPresentAddsFleet verifies the happy path: an
// inspection that finds a devcontainer.json persists the fleet and
// dismisses the dialog.
func TestAddFleetInspectedPresentAddsFleet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	fp := newFleetPage()
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{}},
		fleetPage: fp,
	}

	fp.mode = viewAddFleetInspecting
	fp.dialogPendingFleetName = "alpha"
	fp.dialogPendingRepoURL = "git@example.com:org/alpha.git"

	fp.handleDevcontainerInspected(m, devcontainerInspectedMsg{
		fleetName:       "alpha",
		hasDevcontainer: true,
	})

	if fp.mode != viewNormal {
		t.Fatalf("mode = %v, want viewNormal", fp.mode)
	}
	if _, ok := m.st.Fleets["alpha"]; !ok {
		t.Fatalf("expected fleet alpha to be persisted")
	}
	if fp.dialogPendingFleetName != "" || fp.dialogPendingRepoURL != "" {
		t.Fatalf("pending fields not cleared: %q %q", fp.dialogPendingFleetName, fp.dialogPendingRepoURL)
	}
}

// TestAddFleetInspectedMissingShowsDialog verifies that a "no
// devcontainer" result switches to the choice dialog rather than
// silently persisting the fleet.
func TestAddFleetInspectedMissingShowsDialog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	fp := newFleetPage()
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{}},
		fleetPage: fp,
	}

	fp.mode = viewAddFleetInspecting
	fp.dialogPendingFleetName = "alpha"
	fp.dialogPendingRepoURL = "git@example.com:org/alpha.git"

	fp.handleDevcontainerInspected(m, devcontainerInspectedMsg{
		fleetName:       "alpha",
		hasDevcontainer: false,
	})

	if fp.mode != viewAddFleetNoDevcontainer {
		t.Fatalf("mode = %v, want viewAddFleetNoDevcontainer", fp.mode)
	}
	if _, ok := m.st.Fleets["alpha"]; ok {
		t.Fatalf("fleet was persisted before user chose Abort/Setup")
	}
	// Pending fields must survive — the no-devcontainer dialog reads
	// them to know which fleet to add/launch the agent for.
	if fp.dialogPendingFleetName != "alpha" {
		t.Fatalf("pending fleet name dropped: %q", fp.dialogPendingFleetName)
	}
}

// TestAddFleetNoDevcontainerAbortDoesNotPersist verifies that the
// default Abort action — covered by [a], [n], [enter], and [esc] —
// leaves no trace of the fleet in state.
func TestAddFleetNoDevcontainerAbortDoesNotPersist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, key := range []string{"a", "n", "esc", "enter"} {
		fp := newFleetPage()
		m := &model{
			st:        &state.State{Fleets: map[string]*fleet.Fleet{}},
			fleetPage: fp,
		}
		fp.mode = viewAddFleetNoDevcontainer
		fp.dialogPendingFleetName = "alpha"
		fp.dialogPendingRepoURL = "git@example.com:org/alpha.git"

		var keyMsg tea.KeyMsg
		switch key {
		case "esc":
			keyMsg = tea.KeyMsg{Type: tea.KeyEsc}
		case "enter":
			keyMsg = tea.KeyMsg{Type: tea.KeyEnter}
		default:
			keyMsg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
		}

		fp.updateAddFleetNoDevcontainer(m, keyMsg)

		if fp.mode != viewNormal {
			t.Fatalf("key %q: mode = %v, want viewNormal", key, fp.mode)
		}
		if _, ok := m.st.Fleets["alpha"]; ok {
			t.Fatalf("key %q: fleet was added by Abort path", key)
		}
		if fp.dialogPendingFleetName != "" {
			t.Fatalf("key %q: pending fields not cleared", key)
		}
	}
}

// TestAddFleetInspectStaleResultDropped verifies a result for a
// different fleet (the user dismissed the dialog and started a new
// one in the meantime) is ignored.
func TestAddFleetInspectStaleResultDropped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	fp := newFleetPage()
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{}},
		fleetPage: fp,
	}
	fp.mode = viewAddFleetInspecting
	fp.dialogPendingFleetName = "beta"
	fp.dialogPendingRepoURL = "git@example.com:org/beta.git"

	// Result is for "alpha" — must not mutate state or change the mode.
	fp.handleDevcontainerInspected(m, devcontainerInspectedMsg{
		fleetName:       "alpha",
		hasDevcontainer: true,
	})

	if fp.mode != viewAddFleetInspecting {
		t.Fatalf("mode changed to %v on stale result", fp.mode)
	}
	if _, ok := m.st.Fleets["alpha"]; ok {
		t.Fatalf("stale result persisted unexpected fleet")
	}
}

func TestAddInstanceDialogVimKeysRespectActiveField(t *testing.T) {
	fp := newFleetPage()
	fp.mode = viewAddInstance
	fp.dialogFleet = "alpha"
	fp.dialogBackend = fleet.BackendDevcontainer
	fp.dialogColor = instanceColorWhite
	fp.dialogRow = addInstanceRowName
	fp.dialogFieldActive = true
	fp.textInput.Focus()
	m := &model{}

	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if got := fp.textInput.Value(); got != "k" {
		t.Fatalf("active name input = %q, want k", got)
	}
	if fp.dialogRow != addInstanceRowName {
		t.Fatalf("dialogRow moved while input active: got %d", fp.dialogRow)
	}

	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyEsc})
	if fp.dialogFieldActive {
		t.Fatal("dialogFieldActive should be false after esc from active field")
	}

	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if fp.dialogRow != addInstanceRowBranch {
		t.Fatalf("dialogRow = %d, want branch row", fp.dialogRow)
	}
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if fp.dialogRow != addInstanceRowColor {
		t.Fatalf("dialogRow = %d, want color row", fp.dialogRow)
	}
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if fp.dialogColor != "red" {
		t.Fatalf("dialogColor = %q, want red after l", fp.dialogColor)
	}
	fp.updateAddInstance(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if fp.mode != viewNormal {
		t.Fatalf("mode after q = %v, want viewNormal", fp.mode)
	}
}

func TestEditFleetDialogVimKeysAndActiveHomedir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	stubFleetSettingsSave(t)
	f := &fleet.Fleet{Name: "alpha", Instances: []*fleet.Instance{}}
	fp := newFleetPage()
	fp.mode = viewEditFleet
	fp.dialogFleet = "alpha"
	fp.dialogRow = editFleetRowClaude
	m := &model{
		st:        &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}},
		fleetPage: fp,
	}

	// j moves down through the flat rows; l toggles the focused checkbox.
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if fp.dialogRow != editFleetRowCodex {
		t.Fatalf("dialogRow = %d, want codex row", fp.dialogRow)
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if !fp.dialogCodexMount {
		t.Fatal("l should toggle selected codex row")
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if fp.dialogRow != editFleetRowGh {
		t.Fatalf("dialogRow = %d, want gh row", fp.dialogRow)
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if !fp.dialogGhMount {
		t.Fatal("l should toggle selected gh row")
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if fp.dialogRow != editFleetRowHomeDir {
		t.Fatalf("dialogRow = %d, want home-dir row", fp.dialogRow)
	}

	// Enter activates the home-dir field; typing routes into it; esc discards.
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !fp.dialogFieldActive {
		t.Fatal("enter on home-dir row should activate text field")
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if got := fp.homedirInput.Value(); got != "k" {
		t.Fatalf("active home-dir input = %q, want k", got)
	}
	if fp.dialogRow != editFleetRowHomeDir {
		t.Fatalf("dialogRow moved while home-dir active: got %d", fp.dialogRow)
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyEsc})
	if fp.dialogFieldActive {
		t.Fatal("esc should leave the home-dir field")
	}

	// k now navigates UP (field inactive): home-dir → gh → codex.
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if fp.dialogRow != editFleetRowGh {
		t.Fatalf("dialogRow = %d, want gh row after inactive k", fp.dialogRow)
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if fp.dialogRow != editFleetRowCodex {
		t.Fatalf("dialogRow = %d, want codex row after second inactive k", fp.dialogRow)
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if fp.mode != viewNormal {
		t.Fatalf("mode after q = %v, want viewNormal", fp.mode)
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

// TestMigrateRenamedSessionGroupedReKeysAndPreventsDuplicate covers the #100
// fix: renaming a grouped session changes its group ID, so the saved layout,
// active group, open split, and last-active must all move to the new ID.
// Otherwise the stale ID lingers and buildRows renders it as a duplicate row
// alongside the live renamed group.
func TestMigrateRenamedSessionGroupedReKeysAndPreventsDuplicate(t *testing.T) {
	prevSet, prevDel := setGroupLayoutRemote, deleteGroupLayoutRemote
	setGroupLayoutRemote = func(state.GroupLayout) error { return nil }
	deleteGroupLayoutRemote = func(string, string) error { return nil }
	defer func() { setGroupLayoutRemote, deleteGroupLayoutRemote = prevSet, prevDel }()

	ref := InstanceRef{Fleet: "repo", Instance: "alpha"}
	inst := &fleet.Instance{Name: "alpha", Status: fleet.StatusRunning, ContainerID: "abc"}

	fp := newFleetPage()
	oldKey := computeGroupKey("alpha", "abc123")
	fp.savedGroups[oldKey] = savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123", "alpha~abc123~ff00"},
		PaneCount:    2,
	}
	fp.activeGroup = ActiveGroup{Ref: ref, GroupID: "abc123"}
	fp.splitRef = ref
	fp.splitSession = "alpha~abc123"

	store := NewSessionStore()
	store.SetExpanded(ref, true)
	// Live discovery already reflects the rename: sessions carry the new ID.
	store.SetDiscovery(ref, []tmuxSession{{Name: "alpha~test"}, {Name: "alpha~test~ff00"}})
	store.SetLastActive(ref, lastSession{sessionName: "alpha~abc123", groupID: "abc123"})

	m := &model{
		st: &state.State{
			Fleets:       map[string]*fleet.Fleet{"repo": {Name: "repo", Instances: []*fleet.Instance{inst}}},
			GroupLayouts: map[string]state.GroupLayout{oldKey: {GroupID: "abc123", InstanceName: "alpha", Sessions: []string{"alpha~abc123"}}},
		},
		fleetPage:    fp,
		sessionStore: store,
	}

	m.migrateRenamedSession(sessionRenamedMsg{
		ref:        ref,
		oldName:    "alpha~abc123",
		newName:    "alpha~test",
		oldGroupID: "abc123",
		newGroupID: "test",
	})

	newKey := computeGroupKey("alpha", "test")
	if _, ok := fp.savedGroups[oldKey]; ok {
		t.Fatal("saved group still keyed under old group ID after rename")
	}
	sg, ok := fp.savedGroups[newKey]
	if !ok {
		t.Fatal("saved group not re-keyed to new group ID")
	}
	if sg.GroupID != "test" {
		t.Fatalf("saved group GroupID = %q, want test", sg.GroupID)
	}
	if !slices.Equal(sg.Sessions, []string{"alpha~test", "alpha~test~ff00"}) {
		t.Fatalf("saved group Sessions = %#v, want reprefixed names", sg.Sessions)
	}
	if _, ok := m.st.GroupLayouts[oldKey]; ok {
		t.Fatal("state group layout still keyed under old group ID")
	}
	if _, ok := m.st.GroupLayouts[newKey]; !ok {
		t.Fatal("state group layout not re-keyed to new group ID")
	}
	if fp.activeGroup.GroupID != "test" {
		t.Fatalf("activeGroup.GroupID = %q, want test", fp.activeGroup.GroupID)
	}
	if fp.splitSession != "alpha~test" {
		t.Fatalf("splitSession = %q, want alpha~test", fp.splitSession)
	}
	if last, ok := store.LastActive(ref); !ok || last.groupID != "test" || last.sessionName != "alpha~test" {
		t.Fatalf("lastActive = %#v, want {alpha~test test}", last)
	}

	// After migration the saved group is live, so prune keeps it and buildRows
	// renders exactly one session row for the instance — no duplicate.
	m.pruneSavedGroupsForInstance(ref)
	fp.buildRows(m)
	sessionRows := 0
	for _, r := range fp.rows {
		if r.kind == rowSession {
			sessionRows++
		}
	}
	if sessionRows != 1 {
		t.Fatalf("got %d session rows after rename, want 1 (duplicate not prevented)", sessionRows)
	}
}

// TestMigrateRenamedSessionUngroupedFollowsNewName covers the ungrouped path:
// the pseudo group ID equals the session name, so the split and last-active
// references must track the new name so the shell isn't stranded.
func TestMigrateRenamedSessionUngroupedFollowsNewName(t *testing.T) {
	ref := InstanceRef{Fleet: "repo", Instance: "alpha"}
	fp := newFleetPage()
	fp.activeGroup = ActiveGroup{Ref: ref, GroupID: "foo"}
	fp.splitRef = ref
	fp.splitSession = "foo"

	store := NewSessionStore()
	store.SetExpanded(ref, true)
	store.SetLastActive(ref, lastSession{sessionName: "foo", groupID: "foo"})

	m := &model{
		st:           &state.State{Fleets: map[string]*fleet.Fleet{}},
		fleetPage:    fp,
		sessionStore: store,
	}

	m.migrateRenamedSession(sessionRenamedMsg{ref: ref, oldName: "foo", newName: "bar"})

	if fp.splitSession != "bar" {
		t.Fatalf("splitSession = %q, want bar", fp.splitSession)
	}
	if fp.activeGroup.GroupID != "bar" {
		t.Fatalf("activeGroup.GroupID = %q, want bar", fp.activeGroup.GroupID)
	}
	if last, ok := store.LastActive(ref); !ok || last.sessionName != "bar" || last.groupID != "bar" {
		t.Fatalf("lastActive = %#v, want {bar bar}", last)
	}
}

// TestPruneWithStaleRuntimeDeletesMigratedGroup demonstrates the race condition
// that the sessionRenamedMsg handler avoids by skipping prune. When the runtime
// cache still has OLD session names (stale), pruneSavedGroupsForInstance treats
// the newly re-keyed group as not-live and deletes it.
func TestPruneWithStaleRuntimeDeletesMigratedGroup(t *testing.T) {
	prevSet, prevDel := setGroupLayoutRemote, deleteGroupLayoutRemote
	setGroupLayoutRemote = func(state.GroupLayout) error { return nil }
	deleteGroupLayoutRemote = func(string, string) error { return nil }
	defer func() { setGroupLayoutRemote, deleteGroupLayoutRemote = prevSet, prevDel }()

	ref := InstanceRef{Fleet: "repo", Instance: "alpha"}
	inst := &fleet.Instance{Name: "alpha", Status: fleet.StatusRunning, ContainerID: "abc"}

	fp := newFleetPage()
	oldKey := computeGroupKey("alpha", "abc123")
	fp.savedGroups[oldKey] = savedGroup{
		GroupID:      "abc123",
		InstanceName: "alpha",
		Sessions:     []string{"alpha~abc123", "alpha~abc123~ff00"},
		PaneCount:    2,
	}
	fp.activeGroup = ActiveGroup{Ref: ref, GroupID: "abc123"}
	fp.splitRef = ref
	fp.splitSession = "alpha~abc123"

	store := NewSessionStore()
	store.SetExpanded(ref, true)
	// Stale discovery: still has the OLD session names.
	store.SetDiscovery(ref, []tmuxSession{{Name: "alpha~abc123"}, {Name: "alpha~abc123~ff00"}})
	store.SetLastActive(ref, lastSession{sessionName: "alpha~abc123", groupID: "abc123"})

	m := &model{
		st: &state.State{
			Fleets:       map[string]*fleet.Fleet{"repo": {Name: "repo", Instances: []*fleet.Instance{inst}}},
			GroupLayouts: map[string]state.GroupLayout{oldKey: {GroupID: "abc123", InstanceName: "alpha", Sessions: []string{"alpha~abc123"}}},
		},
		fleetPage:    fp,
		sessionStore: store,
	}

	// Step 1: Migrate re-keys everything from oldGroupID to newGroupID.
	m.migrateRenamedSession(sessionRenamedMsg{
		ref:        ref,
		oldName:    "alpha~abc123",
		newName:    "alpha~test",
		oldGroupID: "abc123",
		newGroupID: "test",
	})

	newKey := computeGroupKey("alpha", "test")
	if _, ok := fp.savedGroups[newKey]; !ok {
		t.Fatal("precondition: saved group should exist under new key after migrate")
	}

	// Step 2: Prune with stale discovery — "test" is NOT in the live set
	// because discovery still only has "abc123" sessions. This deletes the
	// migrated group, which is exactly the bug the handler fix prevents.
	m.pruneSavedGroupsForInstance(ref)

	if _, ok := fp.savedGroups[newKey]; ok {
		t.Fatal("expected prune with stale discovery to delete the migrated group (demonstrates the race)")
	}

	// This confirms that calling prune with stale data is destructive.
	// The fix in sessionRenamedMsg handler skips prune entirely, letting the
	// next periodic runtime refresh (with fresh data) handle pruning safely.
}

// navigateToCacheRow expands the Caching section and moves the cursor onto the
// given cache row (buildkit/deb/image), starting from a freshly opened dialog.
func navigateToCacheRow(t *testing.T, fp *fleetPage, m *model, row int) {
	t.Helper()
	guard := 0
	for fp.dialogRow != editFleetRowCaching {
		fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
		if guard++; guard > 20 {
			t.Fatal("never reached Caching header")
		}
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}) // expand
	guard = 0
	for fp.dialogRow != row {
		fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
		if guard++; guard > 20 {
			t.Fatalf("never reached cache row %d", row)
		}
	}
}

// TestEditFleetSavesDebAndImageCache verifies the two new cache toggles persist
// instantly through the shared Caching interaction model.
func TestEditFleetSavesDebAndImageCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)

	f := &fleet.Fleet{Name: "alpha"}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	navigateToCacheRow(t, fp, m, editFleetRowDebCache)
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace})
	if !fp.dialogDebCache || !f.Settings.DebCacheServer {
		t.Fatalf("deb cache toggle did not persist: dialog=%v saved=%v", fp.dialogDebCache, f.Settings.DebCacheServer)
	}

	navigateToCacheRow(t, fp, m, editFleetRowImageCache)
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeySpace})
	if !fp.dialogImageCache || !f.Settings.ImageCacheServer {
		t.Fatalf("image cache toggle did not persist: dialog=%v saved=%v", fp.dialogImageCache, f.Settings.ImageCacheServer)
	}
}

// TestDeleteCacheCmdDispatchesByKind verifies deleteCacheCmd calls the matching
// per-cache delete RPC for each kind.
func TestDeleteCacheCmdDispatchesByKind(t *testing.T) {
	var bk, deb, img string
	origBK, origDeb, origImg := deleteBuildkitCacheRemote, deleteDebCacheRemote, deleteImageCacheRemote
	deleteBuildkitCacheRemote = func(f string) error { bk = f; return nil }
	deleteDebCacheRemote = func(f string) error { deb = f; return nil }
	deleteImageCacheRemote = func(f string) error { img = f; return nil }
	t.Cleanup(func() {
		deleteBuildkitCacheRemote, deleteDebCacheRemote, deleteImageCacheRemote = origBK, origDeb, origImg
	})

	cases := []struct {
		kind cacheKind
		got  *string
	}{
		{cacheBuildkit, &bk}, {cacheDeb, &deb}, {cacheImage, &img},
	}
	for _, c := range cases {
		msg, ok := deleteCacheCmd(c.kind, "alpha")().(deleteCacheDoneMsg)
		if !ok {
			t.Fatalf("kind %d: cmd did not return deleteCacheDoneMsg", c.kind)
		}
		if msg.kind != c.kind || msg.err != nil {
			t.Fatalf("kind %d: msg = %+v", c.kind, msg)
		}
		if *c.got != "alpha" {
			t.Fatalf("kind %d: matching RPC not called (got %q)", c.kind, *c.got)
		}
	}
}
