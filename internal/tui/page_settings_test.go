package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// TestSettingsSectionIncludesRemoteMcp confirms the new "Fleet Remote (MCP)"
// section is navigable (its two editable items appear) and that the read-only
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
	hasEnabled, hasURL := false, false
	for _, id := range items {
		switch id {
		case settingsItemRemoteMcpEnabled:
			hasEnabled = true
		case settingsItemRemoteMcpGatewayURL:
			hasURL = true
		}
	}
	if !hasEnabled || !hasURL {
		t.Fatalf("remote-mcp items missing from settings nav: enabled=%v url=%v", hasEnabled, hasURL)
	}

	out := sp.viewSettings(m)
	if !strings.Contains(out, "Fleet Remote (MCP)") {
		t.Fatal("settings view missing the Fleet Remote (MCP) section header")
	}
	if !strings.Contains(out, "Public MCP URL") {
		t.Fatal("settings view missing the computed Public MCP URL row when enabled")
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
