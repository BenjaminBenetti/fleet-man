package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// TestSettingsSectionIncludesRemoteMcp confirms the "Fleet MCP" section is
// navigable (copy actions + editable items appear) and that the read-only
// Public MCP URL line renders once the feature is enabled.
func TestSettingsSectionIncludesRemoteMcp(t *testing.T) {
	sp := newSettingsPage()
	cfg := state.DefaultConfig()
	cfg.RemoteMcpSettings.Enabled = true
	m := &model{
		config:     cfg,
		toolStatus: allToolsFound(),
		spinner:    spinner.New(),
	}

	items := sp.visibleItems(m)
	hasEnabled, hasURL, hasCopyLocal, hasCopyRemote := false, false, false, false
	for _, id := range items {
		switch id {
		case settingsItemRemoteMcpEnabled:
			hasEnabled = true
		case settingsItemRemoteMcpGatewayURL:
			hasURL = true
		case settingsItemRemoteMcpCopyLocal:
			hasCopyLocal = true
		case settingsItemRemoteMcpCopyRemote:
			hasCopyRemote = true
		}
	}
	if !hasEnabled || !hasURL {
		t.Fatalf("remote-mcp items missing from settings nav: enabled=%v url=%v", hasEnabled, hasURL)
	}
	// Copy-local is always present; copy-remote appears only when enabled (it is here).
	if !hasCopyLocal || !hasCopyRemote {
		t.Fatalf("copy actions missing from settings nav: local=%v remote=%v", hasCopyLocal, hasCopyRemote)
	}

	out := sp.viewSettings(m)
	if !strings.Contains(out, "Fleet MCP") {
		t.Fatal("settings view missing the Fleet MCP section header")
	}
	if !strings.Contains(out, "Public MCP URL") {
		t.Fatal("settings view missing the computed Public MCP URL row when enabled")
	}
}

// TestDaemonSectionLocalOnly confirms the "Fleet Daemon" / restart action shows
// for a local TUI but is hidden when the TUI is pointed at a remote daemon (it
// can't relaunch a remote process).
func TestDaemonSectionLocalOnly(t *testing.T) {
	sp := newSettingsPage()
	m := &model{
		config:     state.DefaultConfig(),
		toolStatus: allToolsFound(),
		spinner:    spinner.New(),
	}

	// Local (no FLEET_GATEWAY/FLEET_SERVER): the restart row is navigable and rendered.
	if !slices.Contains(sp.visibleItems(m), settingsItemDaemonRestart) {
		t.Fatal("local TUI: Restart daemon row missing from settings nav")
	}
	out := sp.viewSettings(m)
	if !strings.Contains(out, "Fleet Daemon") || !strings.Contains(out, "Restart daemon") {
		t.Fatal("local TUI: settings view missing the Fleet Daemon section / restart row")
	}

	// Remote: the section is hidden.
	t.Setenv("FLEET_GATEWAY", "https://gw.example/abc")
	if slices.Contains(sp.visibleItems(m), settingsItemDaemonRestart) {
		t.Fatal("remote TUI: Restart daemon row must be hidden")
	}
	if strings.Contains(sp.viewSettings(m), "Fleet Daemon") {
		t.Fatal("remote TUI: Fleet Daemon section header must be hidden")
	}
}

// TestDaemonRestartActionInFlight confirms pressing enter on the restart row arms
// the in-flight flag (so the view shows a spinner) and returns a command, and
// that a repeat press while in flight is ignored.
func TestDaemonRestartActionInFlight(t *testing.T) {
	sp := newSettingsPage()
	m := &model{
		config:      state.DefaultConfig(),
		toolStatus:  allToolsFound(),
		currentPage: sp,
		fleetPage:   newFleetPage(),
		spinner:     spinner.New(),
	}
	sp.cursor = settingsPositionOf(sp, m, settingsItemDaemonRestart)
	if sp.cursor < 0 {
		t.Fatal("Restart daemon row not found")
	}

	cmd := sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.daemonRestarting {
		t.Fatal("enter on Restart daemon should set daemonRestarting")
	}
	if cmd == nil {
		t.Fatal("enter on Restart daemon should return a restart command")
	}
	if !strings.Contains(sp.viewSettings(m), "restarting") {
		t.Fatal("in-flight restart should render a 'restarting' spinner row")
	}

	// A second press while the restart is in flight must be a no-op (no new cmd).
	if cmd := sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("repeat enter while restarting should be ignored")
	}
}

// TestSettingsSectionIncludesRemoteFleet confirms the "Enable Remote Fleet"
// toggle is navigable right under "Enable Remote MCP", and that the read-only
// Public GRPC URL line renders once the feature is enabled.
func TestSettingsSectionIncludesRemoteFleet(t *testing.T) {
	sp := newSettingsPage()
	cfg := state.DefaultConfig()
	cfg.RemoteMcpSettings.FleetEnabled = true
	m := &model{
		config:     cfg,
		toolStatus: allToolsFound(),
		spinner:    spinner.New(),
	}

	items := sp.visibleItems(m)
	mcpAt, fleetAt := -1, -1
	for i, id := range items {
		switch id {
		case settingsItemRemoteMcpEnabled:
			mcpAt = i
		case settingsItemRemoteFleetEnabled:
			fleetAt = i
		}
	}
	if fleetAt == -1 {
		t.Fatal("remote-fleet toggle missing from settings nav")
	}
	if fleetAt != mcpAt+1 {
		t.Fatalf("remote-fleet toggle should sit right under remote MCP: mcp=%d fleet=%d", mcpAt, fleetAt)
	}

	out := sp.viewSettings(m)
	if !strings.Contains(out, "Enable Remote Fleet") {
		t.Fatal("settings view missing the Enable Remote Fleet row")
	}
	if !strings.Contains(out, "Public GRPC URL") {
		t.Fatal("settings view missing the computed Public GRPC URL row when enabled")
	}
	// MCP stays off here, so its computed URL row must NOT render.
	if strings.Contains(out, "Public MCP URL") {
		t.Fatal("Public MCP URL row rendered while remote MCP is disabled")
	}
}

// TestCoderVariablesHintScopedToCoderParams guards the footer hint range: the
// "${GIT_URL}" coder-variables hint must show only on coder PARAMETER rows, not
// on the codespaces/browser/remote-mcp rows that sit in the numeric blocks below
// the coder-param range.
func TestCoderVariablesHintScopedToCoderParams(t *testing.T) {
	sp := newSettingsPage()
	cfg := state.DefaultConfig()
	cfg.RemoteMcpSettings.Enabled = true
	cfg.CoderSettings.Parameters = []state.CoderParameter{{Name: "p1", Value: "v1"}}
	m := &model{config: cfg, toolStatus: allToolsFound(), spinner: spinner.New()}

	// Cursor on the Remote MCP gateway-URL row: NO coder-variables hint.
	sp.cursor = settingsPositionOf(sp, m, settingsItemRemoteMcpGatewayURL)
	if out := sp.viewSettings(m); strings.Contains(out, "${GIT_URL}") {
		t.Fatal("coder-variables hint wrongly shown on a remote-mcp row")
	}

	// Cursor on a coder parameter row: hint IS shown.
	sp.cursor = settingsPositionOf(sp, m, settingsItemCoderParamBase)
	if out := sp.viewSettings(m); !strings.Contains(out, "${GIT_URL}") {
		t.Fatal("coder-variables hint missing on a coder parameter row")
	}
}

// TestRemoteMcpStatusValueRendersStates exercises the read-only status line for
// each tunnel state the server can push.
func TestRemoteMcpStatusValueRendersStates(t *testing.T) {
	m := &model{spinner: spinner.New()}

	// No status yet.
	if got := remoteMcpStatusValue(m); !strings.Contains(got, "not connected") {
		t.Fatalf("nil status: want 'not connected', got %q", got)
	}

	m.remoteMcpStatus = &fleetgrpc.RemoteMcpStatus{
		State:     fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED,
		PublicUrl: "https://gw.example.com/mcp/abc123",
	}
	got := remoteMcpStatusValue(m)
	if !strings.Contains(got, "connected") || !strings.Contains(got, "https://gw.example.com/mcp/abc123") {
		t.Fatalf("connected: want 'connected' + url, got %q", got)
	}

	m.remoteMcpStatus = &fleetgrpc.RemoteMcpStatus{State: fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTING}
	if got := remoteMcpStatusValue(m); !strings.Contains(got, "connecting") {
		t.Fatalf("connecting: want 'connecting', got %q", got)
	}

	m.remoteMcpStatus = &fleetgrpc.RemoteMcpStatus{
		State: fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_ERROR,
		Error: "dial tcp: refused",
	}
	if got := remoteMcpStatusValue(m); !strings.Contains(got, "error") || !strings.Contains(got, "dial tcp: refused") {
		t.Fatalf("error: want 'error' + message, got %q", got)
	}
}

// TestRemoteGrpcStatusValueRendersStates exercises the read-only Public GRPC URL
// line for each tunnel state, including a connected tunnel whose gateway
// withheld the gRPC URL (no --public-grpc-url / feature not negotiated).
func TestRemoteGrpcStatusValueRendersStates(t *testing.T) {
	m := &model{spinner: spinner.New()}

	if got := remoteGrpcStatusValue(m); !strings.Contains(got, "not connected") {
		t.Fatalf("nil status: want 'not connected', got %q", got)
	}

	m.remoteMcpStatus = &fleetgrpc.RemoteMcpStatus{
		State:         fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED,
		PublicUrl:     "https://gw.example.com/mcp/abc123",
		PublicGrpcUrl: "https://gw.example.com:50051/grpc/abc123",
	}
	got := remoteGrpcStatusValue(m)
	if !strings.Contains(got, "connected") || !strings.Contains(got, "https://gw.example.com:50051/grpc/abc123") {
		t.Fatalf("connected: want 'connected' + grpc url, got %q", got)
	}

	// Connected but the gateway handed no gRPC URL.
	m.remoteMcpStatus = &fleetgrpc.RemoteMcpStatus{
		State:     fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED,
		PublicUrl: "https://gw.example.com/mcp/abc123",
	}
	if got := remoteGrpcStatusValue(m); !strings.Contains(got, "no public gRPC URL") {
		t.Fatalf("connected without grpc url: want the withheld-URL note, got %q", got)
	}

	m.remoteMcpStatus = &fleetgrpc.RemoteMcpStatus{State: fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTING}
	if got := remoteGrpcStatusValue(m); !strings.Contains(got, "connecting") {
		t.Fatalf("connecting: want 'connecting', got %q", got)
	}

	m.remoteMcpStatus = &fleetgrpc.RemoteMcpStatus{
		State: fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_ERROR,
		Error: "dial tcp: refused",
	}
	if got := remoteGrpcStatusValue(m); !strings.Contains(got, "error") || !strings.Contains(got, "dial tcp: refused") {
		t.Fatalf("error: want 'error' + message, got %q", got)
	}
}

// TestToggleRemoteFleetEnabledPersists confirms pressing enter on the Enable
// Remote Fleet row flips the setting and persists it through the
// setConfigRemote seam, leaving the MCP toggle untouched.
func TestToggleRemoteFleetEnabledPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	origSetConfig := setConfigRemote
	setConfigRemote = func(c *state.Config) error { return state.SaveConfig(c) }
	defer func() { setConfigRemote = origSetConfig }()

	sp := newSettingsPage()
	m := &model{
		config:      state.DefaultConfig(),
		toolStatus:  allToolsFound(),
		currentPage: sp,
		fleetPage:   newFleetPage(),
		spinner:     spinner.New(),
	}
	sp.cursor = settingsPositionOf(sp, m, settingsItemRemoteFleetEnabled)

	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})

	if !m.config.RemoteMcpSettings.FleetEnabled {
		t.Fatal("fleet enabled should be true after toggle")
	}
	if m.config.RemoteMcpSettings.Enabled {
		t.Fatal("toggling remote fleet must not flip remote MCP")
	}
	loaded, err := state.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !loaded.RemoteMcpSettings.FleetEnabled {
		t.Fatal("toggle did not persist to disk")
	}

	// And the cursor stays on the row (no rows were inserted/removed).
	if got := sp.settingsCursorItem(m); got != settingsItemRemoteFleetEnabled {
		t.Fatalf("cursor slid off the fleet toggle: got item %d", got)
	}
}

// TestToggleRemoteMcpEnabledPersists confirms pressing enter on the Enabled row
// flips the setting and persists it through the setConfigRemote seam.
func TestToggleRemoteMcpEnabledPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	origSetConfig := setConfigRemote
	setConfigRemote = func(c *state.Config) error { return state.SaveConfig(c) }
	defer func() { setConfigRemote = origSetConfig }()

	sp := newSettingsPage()
	m := &model{
		config:      state.DefaultConfig(),
		toolStatus:  allToolsFound(),
		currentPage: sp,
		fleetPage:   newFleetPage(),
		spinner:     spinner.New(),
	}
	sp.cursor = settingsPositionOf(sp, m, settingsItemRemoteMcpEnabled)

	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})

	if !m.config.RemoteMcpSettings.Enabled {
		t.Fatal("enabled should be true after toggle")
	}
	loaded, err := state.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !loaded.RemoteMcpSettings.Enabled {
		t.Fatal("toggle did not persist to disk")
	}
}

// TestToggleRemoteMcpKeepsCursor confirms the selection stays on the
// enable/disable row after toggling, even though doing so shows/hides the
// "Copy remote MCP config" row above it and shifts the list.
func TestToggleRemoteMcpKeepsCursor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	origSetConfig := setConfigRemote
	setConfigRemote = func(c *state.Config) error { return state.SaveConfig(c) }
	defer func() { setConfigRemote = origSetConfig }()

	sp := newSettingsPage()
	m := &model{
		config:      state.DefaultConfig(),
		toolStatus:  allToolsFound(),
		currentPage: sp,
		fleetPage:   newFleetPage(),
		spinner:     spinner.New(),
	}
	sp.cursor = settingsPositionOf(sp, m, settingsItemRemoteMcpEnabled)

	// Enable: the copy-remote row appears above, shifting the list.
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := sp.settingsCursorItem(m); got != settingsItemRemoteMcpEnabled {
		t.Fatalf("cursor slid off the enable row after enabling: got item %d", got)
	}

	// Disable: the copy-remote row disappears, shifting the list back.
	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := sp.settingsCursorItem(m); got != settingsItemRemoteMcpEnabled {
		t.Fatalf("cursor slid off the enable row after disabling: got item %d", got)
	}
}

// TestRemoteMcpGatewayURLEditPersists confirms editing the Gateway URL field
// saves the value.
func TestRemoteMcpGatewayURLEditPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	origSetConfig := setConfigRemote
	setConfigRemote = func(c *state.Config) error { return state.SaveConfig(c) }
	defer func() { setConfigRemote = origSetConfig }()

	si := textinput.New()
	si.CharLimit = 256
	sp := &settingsPage{editing: true, input: si}
	m := &model{
		config:      state.DefaultConfig(),
		toolStatus:  allToolsFound(),
		currentPage: sp,
		fleetPage:   newFleetPage(),
		spinner:     spinner.New(),
	}
	sp.cursor = settingsPositionOf(sp, m, settingsItemRemoteMcpGatewayURL)
	sp.input.SetValue("https://gateway.example.com")
	sp.input.Focus()

	sp.Update(m, tea.KeyMsg{Type: tea.KeyEnter})

	if sp.editing {
		t.Fatal("editing should be false after enter")
	}
	if m.config.RemoteMcpSettings.GatewayURL != "https://gateway.example.com" {
		t.Fatalf("GatewayURL = %q", m.config.RemoteMcpSettings.GatewayURL)
	}
	loaded, err := state.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.RemoteMcpSettings.GatewayURL != "https://gateway.example.com" {
		t.Fatalf("persisted GatewayURL = %q", loaded.RemoteMcpSettings.GatewayURL)
	}
}
