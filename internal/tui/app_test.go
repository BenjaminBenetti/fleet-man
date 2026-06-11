package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// allToolsFound returns a toolStatus slice where every tool is marked as found,
// ensuring all settings sections are visible during tests.
func allToolsFound() []deps.ToolStatus {
	return []deps.ToolStatus{
		{Name: "devcontainer", Binary: "devcontainer", Found: true},
		{Name: "gh", Binary: "gh", Found: true},
		{Name: "coder", Binary: "coder", Found: true},
		{Name: "wl-clipboard", Binary: "wl-copy", Found: true},
		{Name: "xclip", Binary: "xclip", Found: true},
	}
}

// settingsPositionOf returns the cursor position for the given item ID
// within the settings page's visible items, or -1 if not found.
func settingsPositionOf(sp *settingsPage, m *model, item int) int {
	for i, id := range sp.visibleItems(m) {
		if id == item {
			return i
		}
	}
	return -1
}

func TestUpdateSettingsEscReturnsToFleetList(t *testing.T) {
	sp := newSettingsPage()
	fp := newFleetPage()
	m := &model{
		currentPage: sp,
		fleetPage:   fp,
		st:          &state.State{Fleets: map[string]*fleet.Fleet{}},
	}

	cmd := sp.Update(m, tea.KeyMsg{Type: tea.KeyEsc})

	// After esc, the model's currentPage should be the fleet page
	if m.currentPage != fp {
		t.Fatalf("currentPage should be fleetPage after esc")
	}
	_ = cmd
}

// TestResumeMsgRedispatchesInnerAndReturnsCmd verifies the resumeMsg handler
// (issue #88): after an execProcess child exits, the wrapped inner message is
// dispatched through Update unchanged AND a command is returned (the mouse
// re-enable batch). Without the handler the inner message would be dropped and
// mouse tracking would stay dead.
func TestResumeMsgRedispatchesInnerAndReturnsCmd(t *testing.T) {
	m := model{
		currentPage: newFleetPage(),
		fleetPage:   newFleetPage(),
		st:          &state.State{Fleets: map[string]*fleet.Fleet{}},
	}

	// Inner WindowSizeMsg has an observable model side-effect (width/height),
	// so we can confirm the handler re-dispatched it rather than swallowing it.
	next, cmd := m.Update(resumeMsg{inner: tea.WindowSizeMsg{Width: 123, Height: 45}})

	nm, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", next)
	}
	if nm.width != 123 || nm.height != 45 {
		t.Fatalf("inner WindowSizeMsg not applied: width=%d height=%d", nm.width, nm.height)
	}
	if cmd == nil {
		t.Fatalf("resumeMsg should return a command (mouse re-enable), got nil")
	}
}

// TestResumeMsgNilInnerReturnsCmd verifies a resumeMsg with no inner message
// still re-enables the mouse (returns a non-nil command).
func TestResumeMsgNilInnerReturnsCmd(t *testing.T) {
	m := model{
		currentPage: newFleetPage(),
		fleetPage:   newFleetPage(),
		st:          &state.State{Fleets: map[string]*fleet.Fleet{}},
	}

	_, cmd := m.Update(resumeMsg{inner: nil})
	if cmd == nil {
		t.Fatalf("resumeMsg with nil inner should still return the mouse re-enable command")
	}
}

// TestUpdateNormalWrapsCursorFromTopToBottom verifies the navigation cycle
// through the Armada selector: up from the top row focuses the selector (the
// cursor leaves the rows), and a second up lands on the bottom row.
func TestUpdateNormalWrapsCursorFromTopToBottom(t *testing.T) {
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},
		{kind: rowInstance, fleetName: "alpha"},
		{kind: rowSettings},
	}
	fp.cursor = 0
	m := &model{fleetPage: fp}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyUp})
	if !fp.armadaFocused {
		t.Fatalf("up from the top row should focus the Armada selector")
	}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyUp})
	if fp.armadaFocused {
		t.Fatalf("up from the Armada selector should unfocus it")
	}
	if fp.cursor != len(fp.rows)-1 {
		t.Fatalf("cursor = %d, want %d", fp.cursor, len(fp.rows)-1)
	}
}

// TestUpdateNormalWrapsCursorFromBottomToTop verifies the inverse cycle: down
// from the bottom row focuses the Armada selector, and a second down lands on
// the top row.
func TestUpdateNormalWrapsCursorFromBottomToTop(t *testing.T) {
	fp := newFleetPage()
	fp.rows = []row{
		{kind: rowFleetHeader, fleetName: "alpha"},
		{kind: rowInstance, fleetName: "alpha"},
		{kind: rowSettings},
	}
	fp.cursor = 2
	m := &model{fleetPage: fp}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyDown})
	if !fp.armadaFocused {
		t.Fatalf("down from the bottom row should focus the Armada selector")
	}

	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyDown})
	if fp.armadaFocused {
		t.Fatalf("down from the Armada selector should unfocus it")
	}
	if fp.cursor != 0 {
		t.Fatalf("cursor = %d, want 0", fp.cursor)
	}
}

func TestUpdateSettingsNavUpDown(t *testing.T) {
	sp := newSettingsPage()
	fp := newFleetPage()
	m := &model{
		config:      state.DefaultConfig(),
		toolStatus:  allToolsFound(),
		currentPage: sp,
		fleetPage:   fp,
	}
	sp.cursor = settingsPositionOf(sp, m, settingsItemTmuxVimKeys)

	// Start on TmuxVimKeys, move down to ShowHelpText
	sp.Update(m, tea.KeyMsg{Type: tea.KeyDown})
	if sp.settingsCursorItem(m) != settingsItemShowHelpText {
		t.Fatalf("item = %d, want %d", sp.settingsCursorItem(m), settingsItemShowHelpText)
	}

	sp.Update(m, tea.KeyMsg{Type: tea.KeyDown})
	if sp.settingsCursorItem(m) != settingsItemDotfilesRepo {
		t.Fatalf("item = %d, want %d", sp.settingsCursorItem(m), settingsItemDotfilesRepo)
	}

	sp.Update(m, tea.KeyMsg{Type: tea.KeyDown})
	if sp.settingsCursorItem(m) != settingsItemDotfilesScript {
		t.Fatalf("item = %d, want %d", sp.settingsCursorItem(m), settingsItemDotfilesScript)
	}

	sp.Update(m, tea.KeyMsg{Type: tea.KeyDown})
	if sp.settingsCursorItem(m) != settingsItemDotfilesAutoInstall {
		t.Fatalf("item = %d, want %d", sp.settingsCursorItem(m), settingsItemDotfilesAutoInstall)
	}

	sp.Update(m, tea.KeyMsg{Type: tea.KeyDown})
	if sp.settingsCursorItem(m) != settingsItemDotfilesSetup {
		t.Fatalf("item = %d, want %d", sp.settingsCursorItem(m), settingsItemDotfilesSetup)
	}

	sp.Update(m, tea.KeyMsg{Type: tea.KeyDown})
	if sp.settingsCursorItem(m) != settingsItemCoderTemplate {
		t.Fatalf("item = %d, want %d", sp.settingsCursorItem(m), settingsItemCoderTemplate)
	}

	sp.Update(m, tea.KeyMsg{Type: tea.KeyDown})
	if sp.settingsCursorItem(m) != settingsItemCoderPreset {
		t.Fatalf("item = %d, want %d", sp.settingsCursorItem(m), settingsItemCoderPreset)
	}

	// Navigate through remaining items until we wrap to top.
	remaining := sp.settingsItemCount(m) - sp.cursor - 1
	for i := 0; i < remaining; i++ {
		sp.Update(m, tea.KeyMsg{Type: tea.KeyDown})
	}

	// Wrap past last item back to first
	sp.Update(m, tea.KeyMsg{Type: tea.KeyDown})
	if sp.cursor != 0 {
		t.Fatalf("cursor = %d, want 0 (wrap to top)", sp.cursor)
	}

	// Wrap up from first goes to last item
	sp.Update(m, tea.KeyMsg{Type: tea.KeyUp})
	wantLast := sp.settingsItemCount(m) - 1
	if sp.cursor != wantLast {
		t.Fatalf("cursor = %d, want %d (wrap to bottom)", sp.cursor, wantLast)
	}
}

func TestUpdateSettingsEnterEditingDotfiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	sp := newSettingsPage()
	fp := newFleetPage()
	m := &model{
		config:      state.DefaultConfig(),
		toolStatus:  allToolsFound(),
		currentPage: sp,
		fleetPage:   fp,
	}
	sp.cursor = settingsPositionOf(sp, m, settingsItemDotfilesRepo)

	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})

	if !sp.editing {
		t.Fatal("editing should be true after enter on dotfiles repo")
	}
	if !sp.input.Focused() {
		t.Fatal("input should be focused")
	}
}

func TestUpdateSettingsEditingSavesOnEnter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// The settings page persists through the server's SetConfig RPC now; stub the
	// seam to the legacy disk write so this test still exercises "edit -> persist"
	// without standing up a server.
	origSetConfig := setConfigRemote
	setConfigRemote = func(c *state.Config) error { return state.SaveConfig(c) }
	defer func() { setConfigRemote = origSetConfig }()

	si := textinput.New()
	si.CharLimit = 256

	sp := &settingsPage{
		editing: true,
		input:   si,
	}
	fp := newFleetPage()
	m := &model{
		config:      state.DefaultConfig(),
		toolStatus:  allToolsFound(),
		currentPage: sp,
		fleetPage:   fp,
	}
	sp.cursor = settingsPositionOf(sp, m, settingsItemDotfilesRepo)
	sp.input.SetValue("https://github.com/user/dotfiles")
	sp.input.Focus()

	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})

	if sp.editing {
		t.Fatal("editing should be false after enter")
	}
	if m.config.DotfilesSettings.RepoURL != "https://github.com/user/dotfiles" {
		t.Fatalf("RepoURL = %q, want %q", m.config.DotfilesSettings.RepoURL, "https://github.com/user/dotfiles")
	}

	config, err := state.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.DotfilesSettings.RepoURL != "https://github.com/user/dotfiles" {
		t.Fatalf("persisted RepoURL = %q, want %q", config.DotfilesSettings.RepoURL, "https://github.com/user/dotfiles")
	}
}

func TestUpdateSettingsEditingCancelsOnEsc(t *testing.T) {
	si := textinput.New()
	si.CharLimit = 256

	config := state.DefaultConfig()
	config.DotfilesSettings.RepoURL = "original"

	sp := &settingsPage{
		editing: true,
		input:   si,
	}
	fp := newFleetPage()
	m := &model{
		config:      config,
		toolStatus:  allToolsFound(),
		currentPage: sp,
		fleetPage:   fp,
	}
	sp.cursor = settingsPositionOf(sp, m, settingsItemDotfilesRepo)
	sp.input.SetValue("changed")
	sp.input.Focus()

	sp.Update(m, tea.KeyMsg{Type: tea.KeyEsc})

	if sp.editing {
		t.Fatal("editing should be false after esc")
	}
	if m.config.DotfilesSettings.RepoURL != "original" {
		t.Fatalf("RepoURL = %q, want %q (should not have changed)", m.config.DotfilesSettings.RepoURL, "original")
	}
}

func TestNeedsDepsCheck(t *testing.T) {
	// Point HOME at an empty dir so ~/.fleet doesn't exist ("first startup"),
	// and clear any ambient remote-endpoint env from the test environment.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_SERVER", "")

	if !needsDepsCheck() {
		t.Fatal("needsDepsCheck() = false on first startup with a local endpoint, want true")
	}

	// Remote endpoints never run the local deps check — the deps live on the
	// machine where fleetd runs, not on the client host.
	t.Setenv("FLEET_GATEWAY", "https://gw.example:50051/abc")
	if needsDepsCheck() {
		t.Fatal("needsDepsCheck() = true with FLEET_GATEWAY set, want false")
	}

	t.Setenv("FLEET_GATEWAY", "")
	t.Setenv("FLEET_SERVER", "remote:50051")
	if needsDepsCheck() {
		t.Fatal("needsDepsCheck() = true with FLEET_SERVER set, want false")
	}

	// Once ~/.fleet exists it is no longer first startup.
	t.Setenv("FLEET_SERVER", "")
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".fleet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if needsDepsCheck() {
		t.Fatal("needsDepsCheck() = true after ~/.fleet exists, want false")
	}
}
