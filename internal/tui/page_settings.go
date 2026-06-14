package tui

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agent"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/doctor"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/aymanbagabas/go-osc52/v2"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ===========================================
// Settings Item Constants
// ===========================================

const (
	settingsItemToolSelection = iota
	settingsItemTmuxVimKeys
	settingsItemShowHelpText
	settingsItemUpdate // only visible when an update is available
	settingsItemDotfilesRepo
	settingsItemDotfilesScript
	settingsItemDotfilesAutoInstall
	settingsItemDotfilesSetup
	settingsItemCoderTemplate
	settingsItemCoderPreset
	settingsItemCoderParamBase // parameters are at index base + i

	settingsItemCodespacesMachine = 500 // codespaces settings start here

	settingsItemBrowserMultiple   = 600 // browser settings start here
	settingsItemBrowserAutoSwitch = 601

	settingsItemRemoteMcpEnabled    = 700 // fleet remote (MCP) settings start here
	settingsItemRemoteMcpGatewayURL = 701
	settingsItemRemoteMcpCopyLocal  = 702 // copy local mcp.json snippet to clipboard
	settingsItemRemoteMcpCopyRemote = 703 // copy gateway mcp.json snippet to clipboard
	settingsItemRemoteFleetEnabled  = 704 // expose the gRPC control surface through the gateway
	settingsItemRemoteMcpPublicURL  = 705 // copy the gateway-assigned public MCP URL
	settingsItemRemoteGrpcPublicURL = 706 // copy the gateway-assigned public gRPC URL
	settingsItemRemoteMcpToken      = 707 // copy the bearer token (~/.fleet/mcp.token)

	// Fleet armada: the "+ Remote Fleet" button has a fixed id; registered
	// remotes get base+i. The base is placed FAR above every other block (and
	// the tool-status/doctor blocks) so an unbounded number of registered
	// remotes can never collide with another item — armada IDs are matched by
	// dedicated checks that run before the `>= settingsItemToolStatusBase`
	// catch-all, so being above 1000 is fine.
	settingsItemArmadaAdd  = 800
	settingsItemArmadaBase = 100000

	settingsItemDaemonRestart = 900 // "Restart daemon" action row (below the tool-status band)

	settingsItemToolStatusBase = 1000 // tool status rows start here
	settingsItemDoctor         = 2000 // doctor action row
	settingsItemKeybindings    = 2001 // keybindings dialog row
)

// isArmadaRemoteItem reports whether an item ID is one of the registered-remote
// rows (base+i). Open-ended: nothing else lives at or above the armada base.
func isArmadaRemoteItem(item int) bool { return item >= settingsItemArmadaBase }

// isCopyRow reports whether an item ID is a Fleet MCP copy action — a row whose
// enter copies to the clipboard rather than editing or cycling a value. Used to
// show the right footer hint and nothing else.
func isCopyRow(item int) bool {
	switch item {
	case settingsItemRemoteMcpCopyLocal, settingsItemRemoteMcpCopyRemote,
		settingsItemRemoteMcpPublicURL, settingsItemRemoteGrpcPublicURL, settingsItemRemoteMcpToken:
		return true
	}
	return false
}

// toolStatusCount is the number of rows rendered in the Tool Status
// section. Must match the length of deps.CheckTools().
const toolStatusCount = 5

// dotfilesSetupPrompt is the instruction sent to the coding agent for
// guided dotfiles setup.
const dotfilesSetupPrompt = "Follow the instructions in https://raw.githubusercontent.com/BenjaminBenetti/Teeleport/main/SETUP_SKILL.md to help me set up Teeleport."

// ===========================================
// Settings Page
// ===========================================

// settingsPage holds settings-page-specific state.
type settingsPage struct {
	cursor          int
	editing         bool
	input           textinput.Model
	showKeybindings bool

	// itemRowYs maps item ID -> terminal Y where the item's first line
	// is rendered. itemHeights maps item ID -> number of lines the item
	// occupies. Both are populated during View() so mouse clicks can be
	// mapped back to the item under the cursor. Only currently-visible
	// (un-scrolled-off) items get an itemRowYs entry.
	itemRowYs   map[int]int
	itemHeights map[int]int

	// scrollOffset is the index of the first content line shown in the
	// scrolling viewport. The mouse wheel adjusts it directly; View()
	// clamps it each render. lastViewCursor is the cursor position at the
	// previous render, used to chase the selection only when it actually
	// moves (so a wheel scroll isn't yanked back to the cursor).
	scrollOffset   int
	lastViewCursor int

	// serverRemote snapshots the remote-gateway settings as last known to be on
	// the server (taken when the page opens, refreshed after each successful
	// save). It tells a real save failure apart from the EXPECTED tunnel bounce:
	// saving a CHANGED remote config from a remote client (FLEET_GATEWAY) tears
	// down the very tunnel the reply rides on, so the RPC reports Unavailable
	// even though the save succeeded server-side.
	serverRemote remoteSettingsSnapshot

	// Fleet Armada section state. The add flow ("+ Remote Fleet") walks
	// URL → token → connection test through the shared text input; the delete
	// button mirrors the edit-fleet cache-clear pattern (horizontal sub-cursor
	// + two-press armed confirm, reset on every cursor move).
	armadaAddStage      armadaAddStage
	armadaAddURL        string // committed URL while the token stage is active
	armadaDeleteFocused bool   // sub-cursor on the [ delete ] button of the current remote row
	armadaDeleteConfirm bool   // "[ delete? ]" armed (first enter on the button)
	armadaBusy          bool   // an add/delete persistence RPC is in flight
}

// armadaAddStage is the "+ Remote Fleet" flow's state machine.
type armadaAddStage int

const (
	armadaAddNone    armadaAddStage = iota
	armadaAddURLIn                  // typing the gateway URL
	armadaAddTokenIn                // typing the bearer token (masked)
	armadaAddTesting                // connection test (then save) in flight
)

// remoteSettingsSnapshot is a comparable copy of the RemoteMcpSettings fields
// that drive the gateway tunnel (the fields whose change makes the server
// bounce it). Declared locally because the TUI must not import internal/state
// and configutil doesn't alias the RemoteMcpSettings type.
type remoteSettingsSnapshot struct {
	mcpEnabled   bool
	fleetEnabled bool
	gatewayURL   string
}

// snapshotRemoteSettings extracts the tunnel-relevant settings from config.
func snapshotRemoteSettings(config *configutil.Config) remoteSettingsSnapshot {
	if config == nil {
		return remoteSettingsSnapshot{}
	}
	return remoteSettingsSnapshot{
		mcpEnabled:   config.RemoteMcpSettings.Enabled,
		fleetEnabled: config.RemoteMcpSettings.FleetEnabled,
		gatewayURL:   config.RemoteMcpSettings.GatewayURL,
	}
}

// remoteSettingsSavedMsg replaces "Failed to save settings" when the failure is
// just the expected tunnel bounce (see remoteSaveBounced).
const remoteSettingsSavedMsg = "Settings saved — remote connection restarting to apply them"

// remoteSaveBounced reports whether a setConfigRemote error is the expected
// side effect of changing the remote-gateway settings from a remote client:
// the save itself succeeded, but applying it restarted the tunnel carrying the
// RPC's reply, so the client saw Unavailable. True only when all three hold:
// the gRPC code is Unavailable, this client is remote (FLEET_GATEWAY /
// FLEET_SERVER), and the attempted save actually changed the remote settings
// relative to the last server config we know of — an unchanged remote config
// never bounces the tunnel (the server's Reconcile is a no-op), so Unavailable
// then is a genuine failure.
func (settingsPage *settingsPage) remoteSaveBounced(m *model, err error) bool {
	if status.Code(err) != codes.Unavailable || !fleetclient.IsRemote() {
		return false
	}
	return snapshotRemoteSettings(m.config) != settingsPage.serverRemote
}

// newSettingsPage creates a new settings page with default state.
func newSettingsPage() *settingsPage {
	input := textinput.New()
	input.CharLimit = 256
	return &settingsPage{
		input:          input,
		itemRowYs:      make(map[int]int),
		itemHeights:    make(map[int]int),
		lastViewCursor: -1,
	}
}

// Init is called when the settings page becomes active.
func (settingsPage *settingsPage) Init(m *model) tea.Cmd {
	// Baseline for remoteSaveBounced: the page opens showing the server's
	// config, so its remote settings are what the server currently runs with.
	settingsPage.serverRemote = snapshotRemoteSettings(m.config)
	// Refresh the armada registry and start the status-sweep tick that keeps
	// the per-remote connection indicators live while the page is open. The
	// armed-flag guard stops a re-entered page from stacking a second loop.
	cmds := []tea.Cmd{fetchArmadaCmd()}
	if !m.armadaTickArmed {
		m.armadaTickArmed = true
		cmds = append(cmds, armadaPingTickCmd())
	}
	return tea.Batch(cmds...)
}

// Update dispatches settings page messages to the appropriate handler.
func (settingsPage *settingsPage) Update(m *model, msg tea.Msg) tea.Cmd {
	if settingsPage.showKeybindings {
		return settingsPage.updateKeybindingsDialog(m, msg)
	}
	if settingsPage.editing {
		return settingsPage.updateSettingsEditing(m, msg)
	}
	if settingsPage.armadaAddStage != armadaAddNone {
		return settingsPage.updateArmadaAdd(m, msg)
	}
	return settingsPage.updateSettingsNav(m, msg)
}

// View renders the settings page.
func (settingsPage *settingsPage) View(m *model) string {
	return settingsPage.viewSettings(m)
}

// ===========================================
// Settings Sections
// ===========================================

// settingsSection defines a titled group of settings rows that can be
// conditionally shown based on tool availability.
type settingsSection struct {
	Title   string               // section header text
	Tool    string               // required tool binary; "" = always visible
	Visible func(m *model) bool  // extra visibility gate; nil = always (subject to Tool)
	Items   func(m *model) []int // returns navigable item IDs for this section
}

// settingsSections lists all settings sections in display order.
var settingsSections = []settingsSection{
	{
		Title: "General",
		Items: func(_ *model) []int {
			return []int{settingsItemTmuxVimKeys, settingsItemShowHelpText, settingsItemUpdate}
		},
	},
	{
		Title: "Dotfiles",
		Items: func(_ *model) []int {
			return []int{settingsItemDotfilesRepo, settingsItemDotfilesScript, settingsItemDotfilesAutoInstall, settingsItemDotfilesSetup}
		},
	},
	{
		Title: "Coder",
		Tool:  "coder",
		Items: func(m *model) []int {
			items := []int{settingsItemCoderTemplate, settingsItemCoderPreset}
			if m.config != nil {
				for i := range m.config.CoderSettings.Parameters {
					items = append(items, settingsItemCoderParamBase+i)
				}
			}
			return items
		},
	},
	{
		Title: "Codespaces",
		Tool:  "gh",
		Items: func(_ *model) []int {
			return []int{settingsItemCodespacesMachine}
		},
	},
	{
		Title: "Browser",
		Items: func(m *model) []int {
			items := []int{settingsItemBrowserMultiple}
			// Auto-switch only applies in shared-profile mode — in
			// per-instance mode there is no "switch" to suppress.
			if m.config != nil && !m.config.BrowserSettings.MultipleBrowsersPerFleetEnabled() {
				items = append(items, settingsItemBrowserAutoSwitch)
			}
			return items
		},
	},
	{
		Title: "Fleet MCP",
		Items: func(m *model) []int {
			// Copy actions come first. The remote-copy action only appears
			// once the gateway tunnel is enabled. The Public MCP URL / Public
			// GRPC URL rows are navigable so enter/click copies the
			// gateway-assigned address; each appears only once its feature is
			// enabled. The Bearer Token copy row joins them whenever either
			// remote surface is on — it's the secret those URLs pair with.
			items := []int{settingsItemRemoteMcpCopyLocal}
			if m.config != nil && m.config.RemoteMcpSettings.Enabled {
				items = append(items, settingsItemRemoteMcpCopyRemote)
			}
			items = append(items, settingsItemRemoteMcpEnabled, settingsItemRemoteFleetEnabled, settingsItemRemoteMcpGatewayURL)
			if m.config != nil && m.config.RemoteMcpSettings.Enabled {
				items = append(items, settingsItemRemoteMcpPublicURL)
			}
			if m.config != nil && m.config.RemoteMcpSettings.FleetEnabled {
				items = append(items, settingsItemRemoteGrpcPublicURL)
			}
			if m.config != nil && (m.config.RemoteMcpSettings.Enabled || m.config.RemoteMcpSettings.FleetEnabled) {
				items = append(items, settingsItemRemoteMcpToken)
			}
			return items
		},
	},
	{
		Title: "Fleet Armada",
		Items: func(m *model) []int {
			// One row per registered remote fleet, then the add button below
			// the list. The registry lives on the model (loaded from the LOCAL
			// daemon), not in config — see armada_client.go.
			items := make([]int, 0, len(m.armadaRemotes)+1)
			for i := range m.armadaRemotes {
				items = append(items, settingsItemArmadaBase+i)
			}
			items = append(items, settingsItemArmadaAdd)
			return items
		},
	},
	{
		Title: "Tool Status",
		Items: func(_ *model) []int {
			items := make([]int, toolStatusCount)
			for i := range items {
				items[i] = settingsItemToolStatusBase + i
			}
			return items
		},
	},
	{
		// Only meaningful for a local TUI: the restart relaunches the LOCAL
		// daemon from the current binary. A remote TUI (FLEET_GATEWAY/
		// FLEET_SERVER) can't relaunch the daemon it talks to, so hide it.
		Title:   "Fleet Daemon",
		Visible: func(_ *model) bool { return !fleetclient.IsRemote() },
		Items: func(_ *model) []int {
			return []int{settingsItemDaemonRestart}
		},
	},
	{
		Title: "Help",
		Items: func(_ *model) []int {
			return []int{settingsItemDoctor, settingsItemKeybindings}
		},
	},
}

// ===========================================
// Navigation Helpers
// ===========================================

// sectionVisible reports whether a settings section should be shown.
func (settingsPage *settingsPage) sectionVisible(m *model, section settingsSection) bool {
	if section.Visible != nil && !section.Visible(m) {
		return false
	}
	if section.Tool == "" {
		return true
	}
	for _, tool := range m.toolStatus {
		if tool.Binary == section.Tool {
			return tool.Found
		}
	}
	return false
}

// visibleItems returns the flat ordered list of navigable item IDs.
func (settingsPage *settingsPage) visibleItems(m *model) []int {
	var items []int
	for _, section := range settingsSections {
		if !settingsPage.sectionVisible(m, section) {
			continue
		}
		for _, id := range section.Items(m) {
			if id == settingsItemUpdate && m.updateAvailable == "" {
				continue
			}
			items = append(items, id)
		}
	}
	return items
}

// cursorToItem moves the cursor onto the given item ID if it is currently
// visible, leaving it unchanged otherwise. Used after a toggle that inserts or
// removes rows (e.g. enabling Remote MCP reveals "Copy remote MCP config") so
// the selection stays on the same logical row instead of sliding when the
// visible-items list changes length.
func (settingsPage *settingsPage) cursorToItem(m *model, item int) {
	for i, id := range settingsPage.visibleItems(m) {
		if id == item {
			settingsPage.cursor = i
			return
		}
	}
}

// settingsCursorItem returns the item ID at the current cursor position.
func (settingsPage *settingsPage) settingsCursorItem(m *model) int {
	items := settingsPage.visibleItems(m)
	if settingsPage.cursor >= 0 && settingsPage.cursor < len(items) {
		return items[settingsPage.cursor]
	}
	return -1
}

// settingsItemCount returns the total number of navigable settings rows.
func (settingsPage *settingsPage) settingsItemCount(m *model) int {
	return len(settingsPage.visibleItems(m))
}

// ===========================================
// Toggle Helpers
// ===========================================

// toggleTmuxVimKeys toggles the tmux vim keys setting.
func (settingsPage *settingsPage) toggleTmuxVimKeys(m *model) {
	if m.config == nil {
		m.config = configutil.DefaultConfig()
	}
	current := m.config.GeneralSettings.TmuxVimKeysEnabled()
	next := !current
	m.config.GeneralSettings.TmuxVimKeys = &next
	if err := setConfigRemote(m.config); err != nil {
		m.config.GeneralSettings.TmuxVimKeys = &current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	label := "off"
	if next {
		label = "on"
	}
	m.message = fmt.Sprintf("Tmux vim keys set to %s", label)
}

// toggleShowHelpText flips the show-help-text preference and saves.
func (settingsPage *settingsPage) toggleShowHelpText(m *model) {
	if m.config == nil {
		m.config = configutil.DefaultConfig()
	}
	current := m.config.GeneralSettings.ShowHelpTextEnabled()
	next := !current
	m.config.GeneralSettings.ShowHelpText = &next
	if err := setConfigRemote(m.config); err != nil {
		m.config.GeneralSettings.ShowHelpText = &current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	label := "off"
	if next {
		label = "on"
	}
	m.message = fmt.Sprintf("Show help text set to %s", label)
}

// toggleBrowserAutoSwitch flips the "Auto Switch" preference and saves.
// When on, the browser-switch confirmation dialog is suppressed and any
// running browser bound to the target data dir is killed+relaunched
// silently.
func (settingsPage *settingsPage) toggleBrowserAutoSwitch(m *model) {
	if m.config == nil {
		m.config = configutil.DefaultConfig()
	}
	current := m.config.BrowserSettings.AutoSwitchEnabled()
	next := !current
	m.config.BrowserSettings.AutoSwitch = &next
	if err := setConfigRemote(m.config); err != nil {
		m.config.BrowserSettings.AutoSwitch = &current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	label := "off"
	if next {
		label = "on"
	}
	m.message = fmt.Sprintf("Auto switch set to %s", label)
}

// toggleBrowserMultiple flips the "Enable Multiple Browsers Per Fleet"
// preference and saves. When on, each instance gets its own browser
// data dir under <fleet>/<instance>/.browser instead of sharing a
// single profile under <fleet>/.browser.
func (settingsPage *settingsPage) toggleBrowserMultiple(m *model) {
	if m.config == nil {
		m.config = configutil.DefaultConfig()
	}
	current := m.config.BrowserSettings.MultipleBrowsersPerFleetEnabled()
	next := !current
	m.config.BrowserSettings.MultipleBrowsersPerFleet = &next
	if err := setConfigRemote(m.config); err != nil {
		m.config.BrowserSettings.MultipleBrowsersPerFleet = &current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	label := "off"
	if next {
		label = "on"
	}
	m.message = fmt.Sprintf("Multiple browsers per fleet set to %s", label)
}

// toggleRemoteMcpEnabled flips the "Enabled" preference for exposing this
// daemon's MCP server through a remote fleet gateway, and saves. Reverts on a
// save failure, mirroring the other toggles.
func (settingsPage *settingsPage) toggleRemoteMcpEnabled(m *model) {
	if m.config == nil {
		m.config = configutil.DefaultConfig()
	}
	current := m.config.RemoteMcpSettings.Enabled
	next := !current
	m.config.RemoteMcpSettings.Enabled = next
	if err := setConfigRemote(m.config); err != nil {
		if settingsPage.remoteSaveBounced(m, err) {
			// Saved server-side; the error was the tunnel restarting under us.
			settingsPage.serverRemote = snapshotRemoteSettings(m.config)
			settingsPage.cursorToItem(m, settingsItemRemoteMcpEnabled)
			m.message = remoteSettingsSavedMsg
			return
		}
		m.config.RemoteMcpSettings.Enabled = current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	settingsPage.serverRemote = snapshotRemoteSettings(m.config)
	// Toggling shows/hides the "Copy remote MCP config" row above this one, which
	// shifts the list. Re-pin the cursor on the enable/disable row so it doesn't
	// slide onto a neighbouring row.
	settingsPage.cursorToItem(m, settingsItemRemoteMcpEnabled)
	label := "off"
	if next {
		label = "on"
	}
	m.message = fmt.Sprintf("Remote MCP set to %s", label)
}

// toggleRemoteFleetEnabled flips the "Enable Remote Fleet" preference — exposing
// this daemon's gRPC control surface through the gateway so a remote `fleet`
// binary can drive it — and saves. Reverts on a save failure, mirroring the
// other toggles. Toggling this shows/hides the Public GRPC URL and Bearer Token
// rows, but both sit BELOW this toggle in the Fleet MCP section, so this row's
// index is preserved and no cursor re-pin is needed — unlike the MCP toggle,
// whose Copy-remote row sits ABOVE it. Re-pin (cursorToItem) if that ordering
// ever changes.
func (settingsPage *settingsPage) toggleRemoteFleetEnabled(m *model) {
	if m.config == nil {
		m.config = configutil.DefaultConfig()
	}
	current := m.config.RemoteMcpSettings.FleetEnabled
	next := !current
	m.config.RemoteMcpSettings.FleetEnabled = next
	if err := setConfigRemote(m.config); err != nil {
		if settingsPage.remoteSaveBounced(m, err) {
			// Saved server-side; the error was the tunnel restarting under us.
			settingsPage.serverRemote = snapshotRemoteSettings(m.config)
			m.message = remoteSettingsSavedMsg
			return
		}
		m.config.RemoteMcpSettings.FleetEnabled = current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	settingsPage.serverRemote = snapshotRemoteSettings(m.config)
	label := "off"
	if next {
		label = "on"
	}
	m.message = fmt.Sprintf("Remote Fleet set to %s", label)
}

// toggleAutoInstall toggles the dotfiles auto-install setting.
func (settingsPage *settingsPage) toggleAutoInstall(m *model) {
	if m.config == nil {
		m.config = configutil.DefaultConfig()
	}
	m.config.DotfilesSettings.AutoInstall = !m.config.DotfilesSettings.AutoInstall
	if err := setConfigRemote(m.config); err != nil {
		m.config.DotfilesSettings.AutoInstall = !m.config.DotfilesSettings.AutoInstall
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	label := "off"
	if m.config.DotfilesSettings.AutoInstall {
		label = "on"
	}
	m.message = fmt.Sprintf("Auto install dotfiles set to %s", label)
}

// cycleCoderPreset cycles through available coder presets.
func (settingsPage *settingsPage) cycleCoderPreset(m *model, direction int) {
	if m.config == nil || len(m.coderPresets) == 0 {
		return
	}
	current := m.config.CoderSettings.Preset
	idx := 0
	for i, preset := range m.coderPresets {
		if preset == current {
			idx = i
			break
		}
	}
	idx = (idx + direction + len(m.coderPresets)) % len(m.coderPresets)
	m.config.CoderSettings.Preset = m.coderPresets[idx]
	if err := setConfigRemote(m.config); err != nil {
		m.config.CoderSettings.Preset = current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	m.message = fmt.Sprintf("Preset set to %s", m.config.CoderSettings.Preset)
}

// cycleCodespacesMachine cycles through available codespace machine types.
func (settingsPage *settingsPage) cycleCodespacesMachine(m *model, direction int) {
	if m.config == nil || len(m.codespaceMachines) == 0 {
		return
	}
	current := m.config.CodespacesSettings.Machine
	idx := 0
	for i, machine := range m.codespaceMachines {
		if machine.Name == current {
			idx = i
			break
		}
	}
	idx = (idx + direction + len(m.codespaceMachines)) % len(m.codespaceMachines)
	selected := m.codespaceMachines[idx]
	m.config.CodespacesSettings.Machine = selected.Name
	if err := setConfigRemote(m.config); err != nil {
		m.config.CodespacesSettings.Machine = current
		m.message = fmt.Sprintf("Failed to save settings: %v", err)
		return
	}
	m.message = fmt.Sprintf("Machine set to %s", selected.DisplayName)
}

// codespacesMachineLabel returns the display label for the currently
// configured machine.
func (settingsPage *settingsPage) codespacesMachineLabel(m *model) string {
	name := m.config.CodespacesSettings.Machine
	for _, machine := range m.codespaceMachines {
		if machine.Name == name {
			return machine.DisplayName
		}
	}
	return name
}

// remoteMcpStatusValue renders the Public MCP URL / connection-state
// line from the latest status the server pushed over Watch. The tunnel itself
// lands in a later PR, so today this resolves to "not connected" once enabled;
// the CONNECTING/CONNECTED/ERROR rendering is wired and ready for it.
func remoteMcpStatusValue(m *model) string {
	st := m.remoteMcpStatus
	if st == nil {
		return dimStyle.Render("(not connected)")
	}
	switch st.GetState() {
	case fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED:
		if url := st.GetPublicUrl(); url != "" {
			return statusRunningStyle.Render("connected") + "  " + url
		}
		return statusRunningStyle.Render("connected")
	case fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTING:
		return m.spinner.View() + " connecting…"
	case fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_ERROR:
		msg := st.GetError()
		if msg == "" {
			msg = "connection failed"
		}
		return statusCreatingStyle.Render("error") + "  " + dimStyle.Render(msg)
	default: // UNSPECIFIED / not yet connected
		return dimStyle.Render("(not connected)")
	}
}

// remoteMcpPublicURL returns the live gateway-assigned Public MCP URL, or ""
// when the tunnel is not (yet) connected.
func remoteMcpPublicURL(m *model) string {
	if m.remoteMcpStatus == nil {
		return ""
	}
	return m.remoteMcpStatus.GetPublicUrl()
}

// remoteGrpcPublicURL returns the live gateway-assigned Public GRPC URL, or ""
// when the tunnel is not (yet) connected or the gateway withheld it.
func remoteGrpcPublicURL(m *model) string {
	if m.remoteMcpStatus == nil {
		return ""
	}
	return m.remoteMcpStatus.GetPublicGrpcUrl()
}

// remoteGrpcStatusValue renders the Public GRPC URL line from the same
// pushed tunnel status as remoteMcpStatusValue (one tunnel carries both traffic
// kinds, so they share connection state). A connected tunnel with no gRPC URL
// means the gateway withheld it — it is old, runs without --public-grpc-url, or
// registered this session before remote fleet was enabled (the reconnect that
// negotiates grpc refreshes it).
func remoteGrpcStatusValue(m *model) string {
	st := m.remoteMcpStatus
	if st == nil {
		return dimStyle.Render("(not connected)")
	}
	switch st.GetState() {
	case fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED:
		if url := st.GetPublicGrpcUrl(); url != "" {
			return statusRunningStyle.Render("connected") + "  " + url
		}
		return statusRunningStyle.Render("connected") + "  " + dimStyle.Render("(gateway provided no public gRPC URL)")
	case fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTING:
		return m.spinner.View() + " connecting…"
	case fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_ERROR:
		msg := st.GetError()
		if msg == "" {
			msg = "connection failed"
		}
		return statusCreatingStyle.Render("error") + "  " + dimStyle.Render(msg)
	default: // UNSPECIFIED / not yet connected
		return dimStyle.Render("(not connected)")
	}
}

// localMcpConfigJSON returns the mcp.json snippet for the loopback MCP server,
// matching the README. It uses the ${FLEET_MCP_URL}/${FLEET_MCP_TOKEN} env vars
// written to ~/.fleet/mcp.env so the snippet survives port changes.
func localMcpConfigJSON() string {
	return `{
  "mcpServers": {
    "fleet": {
      "type": "http",
      "url": "${FLEET_MCP_URL}",
      "headers": { "Authorization": "Bearer ${FLEET_MCP_TOKEN}" }
    }
  }
}`
}

// remoteMcpConfigJSON returns the mcp.json snippet for reaching this fleet
// through the gateway, using the live gateway-assigned Public MCP URL. The
// bearer token is left as a placeholder: a remote machine won't have
// ~/.fleet/mcp.env, so the token must be pasted from ~/.fleet/mcp.token.
func remoteMcpConfigJSON(publicURL string) string {
	return fmt.Sprintf(`{
  "mcpServers": {
    "fleet-remote": {
      "type": "http",
      "url": %q,
      "headers": { "Authorization": "Bearer <token from ~/.fleet/mcp.token>" }
    }
  }
}`, publicURL)
}

// copyToClipboardCmd copies content to the terminal clipboard via OSC 52. We
// write to stderr (not bubbletea's stdout renderer) to avoid interleaving with
// a frame, and emit plain OSC 52: the TUI runs inside tmux with
// `set-clipboard on`, so tmux consumes the sequence directly (no passthrough).
func copyToClipboardCmd(content string) tea.Cmd {
	return func() tea.Msg {
		_, _ = osc52.New(content).WriteTo(os.Stderr)
		return nil
	}
}

// ===========================================
// Update Handlers
// ===========================================

// updateKeybindingsDialog handles input while the keybindings overlay is shown.
func (settingsPage *settingsPage) updateKeybindingsDialog(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q", "ctrl+c":
			settingsPage.showKeybindings = false
		}
	}
	return nil
}

// updateSettingsNav handles keyboard navigation in the settings page.
func (settingsPage *settingsPage) updateSettingsNav(m *model, msg tea.Msg) tea.Cmd {
	count := settingsPage.settingsItemCount(m)

	switch msg := msg.(type) {
	case tea.KeyMsg:
		m.message = ""

		switch msg.String() {
		case "esc", "q":
			return m.ChangeRoute(routeFleetList)

		case "ctrl+c", "ctrl+q":
			m.quitting = true
			return tea.Quit

		case "up", "k":
			settingsPage.cursor = (settingsPage.cursor - 1 + count) % count
			// Leaving a remote-fleet row resets its delete sub-cursor, exactly
			// like the edit-fleet cache rows.
			settingsPage.armadaDeleteFocused = false
			settingsPage.armadaDeleteConfirm = false
			return nil

		case "down", "j":
			settingsPage.cursor = (settingsPage.cursor + 1) % count
			settingsPage.armadaDeleteFocused = false
			settingsPage.armadaDeleteConfirm = false
			return nil

		case "left", "h":
			item := settingsPage.settingsCursorItem(m)
			if isArmadaRemoteItem(item) {
				// Back off the [ delete ] button onto the row itself.
				settingsPage.armadaDeleteFocused = false
				settingsPage.armadaDeleteConfirm = false
				return nil
			}
			if item == settingsItemTmuxVimKeys {
				settingsPage.toggleTmuxVimKeys(m)
			} else if item == settingsItemShowHelpText {
				settingsPage.toggleShowHelpText(m)
			} else if item == settingsItemDotfilesAutoInstall {
				settingsPage.toggleAutoInstall(m)
			} else if item == settingsItemBrowserMultiple {
				settingsPage.toggleBrowserMultiple(m)
			} else if item == settingsItemBrowserAutoSwitch {
				settingsPage.toggleBrowserAutoSwitch(m)
			} else if item == settingsItemRemoteMcpEnabled {
				settingsPage.toggleRemoteMcpEnabled(m)
			} else if item == settingsItemRemoteFleetEnabled {
				settingsPage.toggleRemoteFleetEnabled(m)
			} else if item == settingsItemCoderPreset {
				settingsPage.cycleCoderPreset(m, -1)
			} else if item == settingsItemCodespacesMachine {
				settingsPage.cycleCodespacesMachine(m, -1)
			}
			return nil

		case "right", "l":
			item := settingsPage.settingsCursorItem(m)
			if isArmadaRemoteItem(item) {
				// Focus the row's [ delete ] button (cache-clear UX pattern).
				settingsPage.armadaDeleteFocused = true
				return nil
			}
			if item == settingsItemTmuxVimKeys {
				settingsPage.toggleTmuxVimKeys(m)
			} else if item == settingsItemShowHelpText {
				settingsPage.toggleShowHelpText(m)
			} else if item == settingsItemDotfilesAutoInstall {
				settingsPage.toggleAutoInstall(m)
			} else if item == settingsItemBrowserMultiple {
				settingsPage.toggleBrowserMultiple(m)
			} else if item == settingsItemBrowserAutoSwitch {
				settingsPage.toggleBrowserAutoSwitch(m)
			} else if item == settingsItemRemoteMcpEnabled {
				settingsPage.toggleRemoteMcpEnabled(m)
			} else if item == settingsItemRemoteFleetEnabled {
				settingsPage.toggleRemoteFleetEnabled(m)
			} else if item == settingsItemCoderPreset {
				settingsPage.cycleCoderPreset(m, 1)
			} else if item == settingsItemCodespacesMachine {
				settingsPage.cycleCodespacesMachine(m, 1)
			}
			return nil

		case "enter", " ":
			item := settingsPage.settingsCursorItem(m)
			if item == settingsItemArmadaAdd {
				return settingsPage.beginArmadaAdd(m)
			}
			if isArmadaRemoteItem(item) {
				return settingsPage.enterArmadaRemoteRow(m, item-settingsItemArmadaBase)
			}
			if item == settingsItemTmuxVimKeys {
				settingsPage.toggleTmuxVimKeys(m)
				return nil
			}
			if item == settingsItemShowHelpText {
				settingsPage.toggleShowHelpText(m)
				return nil
			}
			if item == settingsItemDotfilesAutoInstall {
				settingsPage.toggleAutoInstall(m)
				return nil
			}
			if item == settingsItemBrowserMultiple {
				settingsPage.toggleBrowserMultiple(m)
				return nil
			}
			if item == settingsItemBrowserAutoSwitch {
				settingsPage.toggleBrowserAutoSwitch(m)
				return nil
			}
			if item == settingsItemRemoteMcpEnabled {
				settingsPage.toggleRemoteMcpEnabled(m)
				return nil
			}
			if item == settingsItemRemoteFleetEnabled {
				settingsPage.toggleRemoteFleetEnabled(m)
				return nil
			}
			if item == settingsItemRemoteMcpCopyLocal {
				m.message = "Local MCP config copied to clipboard"
				return copyToClipboardCmd(localMcpConfigJSON())
			}
			if item == settingsItemRemoteMcpCopyRemote {
				url := remoteMcpPublicURL(m)
				if url == "" {
					m.message = "No public URL yet — connect to the gateway first"
					return nil
				}
				m.message = "Remote MCP config copied to clipboard"
				return copyToClipboardCmd(remoteMcpConfigJSON(url))
			}
			if item == settingsItemRemoteMcpPublicURL {
				url := remoteMcpPublicURL(m)
				if url == "" {
					m.message = "No public MCP URL yet — connect to the gateway first"
					return nil
				}
				m.message = "Public MCP URL copied to clipboard"
				return copyToClipboardCmd(url)
			}
			if item == settingsItemRemoteGrpcPublicURL {
				url := remoteGrpcPublicURL(m)
				if url == "" {
					m.message = "No public GRPC URL yet — connect to the gateway first"
					return nil
				}
				m.message = "Public GRPC URL copied to clipboard"
				return copyToClipboardCmd(url)
			}
			if item == settingsItemRemoteMcpToken {
				token := fleetclient.BearerToken()
				if token == "" {
					m.message = "No bearer token found (set FLEET_TOKEN or ~/.fleet/mcp.token)"
					return nil
				}
				m.message = "Bearer token copied to clipboard"
				return copyToClipboardCmd(token)
			}
			if item == settingsItemCoderPreset {
				settingsPage.cycleCoderPreset(m, 1)
			}
			if item == settingsItemCodespacesMachine {
				settingsPage.cycleCodespacesMachine(m, 1)
				return nil
			}
			if item == settingsItemUpdate {
				return performUpdateCmd()
			}
			if item == settingsItemDoctor {
				cmd, err := doctor.Command()
				if err != nil {
					m.message = err.Error()
					return nil
				}
				return execProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })
			}
			if item == settingsItemKeybindings {
				settingsPage.showKeybindings = true
				return nil
			}
			if item == settingsItemDaemonRestart {
				if m.daemonRestarting {
					return nil // already in flight; ignore repeat presses
				}
				m.daemonRestarting = true
				m.message = "Restarting fleet daemon…"
				return restartDaemonCmd()
			}
			if item == settingsItemDotfilesSetup {
				cmd, err := agent.CommandWithPrompt(dotfilesSetupPrompt)
				if err != nil {
					m.message = err.Error()
					return nil
				}
				return execProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })
			}
			if item >= settingsItemToolStatusBase {
				idx := item - settingsItemToolStatusBase
				if idx < len(m.toolStatus) {
					openURL(m.toolStatus[idx].InstallURL)
					m.message = fmt.Sprintf("Opening %s", m.toolStatus[idx].InstallURL)
				}
				return nil
			}
			return settingsPage.enterSettingsEditing(m)
		}
	}

	return nil
}

// enterSettingsEditing activates text editing for the current setting.
func (settingsPage *settingsPage) enterSettingsEditing(m *model) tea.Cmd {
	if m.config == nil {
		m.config = configutil.DefaultConfig()
	}

	item := settingsPage.settingsCursorItem(m)
	var current string
	switch {
	case item == settingsItemDotfilesRepo:
		current = m.config.DotfilesSettings.RepoURL
		settingsPage.input.Placeholder = "https://github.com/user/dotfiles"
	case item == settingsItemDotfilesScript:
		current = m.config.DotfilesSettings.InstallScript
		settingsPage.input.Placeholder = "install.sh"
	case item == settingsItemCoderTemplate:
		current = m.config.CoderSettings.Template
		settingsPage.input.Placeholder = "template-name"
	case item == settingsItemRemoteMcpGatewayURL:
		current = m.config.RemoteMcpSettings.GatewayURL
		settingsPage.input.Placeholder = "https://gateway.example.com"
	case item >= settingsItemCoderParamBase && item < settingsItemCodespacesMachine:
		idx := item - settingsItemCoderParamBase
		if idx < len(m.config.CoderSettings.Parameters) {
			current = m.config.CoderSettings.Parameters[idx].Value
			param := m.config.CoderSettings.Parameters[idx]
			if param.DefaultValue != "" {
				settingsPage.input.Placeholder = param.DefaultValue
			} else {
				settingsPage.input.Placeholder = "value"
			}
		}
	case item == settingsItemCodespacesMachine:
		settingsPage.cycleCodespacesMachine(m, 1)
		return nil
	default:
		return nil
	}

	settingsPage.editing = true
	settingsPage.input.SetValue(current)
	settingsPage.input.Focus()
	settingsPage.input.CursorEnd()
	return settingsPage.input.Cursor.BlinkCmd()
}

// ===========================================
// Fleet Armada handlers
// ===========================================

// beginArmadaAdd starts the "+ Remote Fleet" flow at the URL stage.
func (settingsPage *settingsPage) beginArmadaAdd(m *model) tea.Cmd {
	if settingsPage.armadaBusy {
		return nil
	}
	settingsPage.armadaAddStage = armadaAddURLIn
	settingsPage.armadaAddURL = ""
	settingsPage.input.Placeholder = "https://gateway.example.com/<session-id>"
	settingsPage.input.EchoMode = textinput.EchoNormal
	settingsPage.input.SetValue("")
	settingsPage.input.Focus()
	return settingsPage.input.Cursor.BlinkCmd()
}

// cancelArmadaAdd resets the add flow and restores the shared input.
func (settingsPage *settingsPage) cancelArmadaAdd() {
	settingsPage.armadaAddStage = armadaAddNone
	settingsPage.armadaAddURL = ""
	settingsPage.input.SetValue("")
	settingsPage.input.EchoMode = textinput.EchoNormal
	settingsPage.input.Blur()
}

// enterArmadaRemoteRow handles enter/space on a registered remote's row: on
// the row itself it re-pings the remote; on the focused [ delete ] button it
// arms the confirm, then removes the remote on the second press.
func (settingsPage *settingsPage) enterArmadaRemoteRow(m *model, idx int) tea.Cmd {
	if idx < 0 || idx >= len(m.armadaRemotes) || settingsPage.armadaBusy {
		return nil
	}
	remote := m.armadaRemotes[idx]

	if settingsPage.armadaDeleteFocused {
		if !settingsPage.armadaDeleteConfirm {
			settingsPage.armadaDeleteConfirm = true
			return nil
		}
		settingsPage.armadaBusy = true
		next := slices.Delete(slices.Clone(m.armadaRemotes), idx, idx+1)
		return saveArmadaCmd(next, "removed", idx)
	}

	// Plain enter on the row: probe it again right now.
	m.armadaStatus[remote.URL] = armadaStatus{state: armadaStatusPinging}
	return pingArmadaCmd(remote.URL, remote.Token)
}

// updateArmadaAdd handles input while the "+ Remote Fleet" flow is active.
func (settingsPage *settingsPage) updateArmadaAdd(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		settingsPage.input, cmd = settingsPage.input.Update(msg)
		return cmd
	}

	// While the connection test runs, only quitting is allowed; the result
	// message finishes (or fails) the flow.
	if settingsPage.armadaAddStage == armadaAddTesting {
		switch keyMsg.String() {
		case "ctrl+c", "ctrl+q":
			m.quitting = true
			return tea.Quit
		}
		return nil
	}

	switch keyMsg.Type {
	case tea.KeyEnter:
		switch settingsPage.armadaAddStage {
		case armadaAddURLIn:
			url := strings.TrimSpace(settingsPage.input.Value())
			if url == "" {
				m.message = "Enter the remote fleet's gateway URL"
				return nil
			}
			for _, r := range m.armadaRemotes {
				if r.URL == url {
					m.message = "That remote fleet is already registered"
					return nil
				}
			}
			settingsPage.armadaAddURL = url
			settingsPage.armadaAddStage = armadaAddTokenIn
			settingsPage.input.SetValue("")
			settingsPage.input.Placeholder = "bearer token (~/.fleet/mcp.token on the remote)"
			settingsPage.input.EchoMode = textinput.EchoPassword
			return settingsPage.input.Cursor.BlinkCmd()

		case armadaAddTokenIn:
			token := strings.TrimSpace(settingsPage.input.Value())
			if token == "" {
				m.message = "Enter the remote fleet's bearer token"
				return nil
			}
			settingsPage.armadaAddStage = armadaAddTesting
			settingsPage.input.Blur()
			settingsPage.input.EchoMode = textinput.EchoNormal
			m.message = ""
			return testArmadaRemoteCmd(settingsPage.armadaAddURL, token)
		}
		return nil

	case tea.KeyEsc:
		settingsPage.cancelArmadaAdd()
		m.message = "Cancelled"
		return nil
	}

	var cmd tea.Cmd
	settingsPage.input, cmd = settingsPage.input.Update(msg)
	return cmd
}

// armadaStatusValue renders one remote's connection indicator from the latest
// ping outcome.
func armadaStatusValue(m *model, url string) string {
	st := m.armadaStatus[url]
	switch st.state {
	case armadaStatusConnected:
		return statusRunningStyle.Render("connected")
	case armadaStatusPinging:
		return m.spinner.View() + " pinging…"
	case armadaStatusError:
		return statusCreatingStyle.Render("error") + "  " + dimStyle.Render(st.err)
	default:
		return dimStyle.Render("(status unknown)")
	}
}

// renderArmadaDeleteButton renders a remote row's [ delete ] button with the
// focus/armed states of the cache-clear pattern.
func (settingsPage *settingsPage) renderArmadaDeleteButton(m *model, rowActive bool) string {
	focused := rowActive && settingsPage.armadaDeleteFocused
	label := "[ delete ]"
	if settingsPage.armadaBusy && focused {
		return m.spinner.View() + " removing…"
	}
	if focused && settingsPage.armadaDeleteConfirm {
		label = "[ delete? ]"
	}
	if focused {
		return selectedStyle.Render(label)
	}
	return dimStyle.Render(label)
}

// updateSettingsEditing handles input while editing a text field.
func (settingsPage *settingsPage) updateSettingsEditing(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEnter:
			value := strings.TrimSpace(settingsPage.input.Value())
			if m.config == nil {
				m.config = configutil.DefaultConfig()
			}

			item := settingsPage.settingsCursorItem(m)
			var cmd tea.Cmd
			switch {
			case item == settingsItemDotfilesRepo:
				m.config.DotfilesSettings.RepoURL = value
			case item == settingsItemDotfilesScript:
				m.config.DotfilesSettings.InstallScript = value
			case item == settingsItemRemoteMcpGatewayURL:
				m.config.RemoteMcpSettings.GatewayURL = value
			case item == settingsItemCoderTemplate:
				oldTemplate := m.config.CoderSettings.Template
				m.config.CoderSettings.Template = value
				if value != "" && value != oldTemplate {
					m.coderFetchingParams = true
					m.message = "Fetching template parameters..."
					cmd = fetchCoderParamsCmd(value)
				}
			case item >= settingsItemCoderParamBase && item < settingsItemCodespacesMachine:
				idx := item - settingsItemCoderParamBase
				if idx < len(m.config.CoderSettings.Parameters) {
					m.config.CoderSettings.Parameters[idx].Value = value
				}
			}

			if err := setConfigRemote(m.config); err != nil {
				if settingsPage.remoteSaveBounced(m, err) {
					// Editing the Gateway URL from a remote client: the save
					// landed, then restarting the tunnel killed the reply.
					settingsPage.serverRemote = snapshotRemoteSettings(m.config)
					m.message = remoteSettingsSavedMsg
				} else {
					m.message = fmt.Sprintf("Failed to save settings: %v", err)
				}
			} else {
				settingsPage.serverRemote = snapshotRemoteSettings(m.config)
				if cmd == nil {
					m.message = "Saved"
				}
			}
			settingsPage.editing = false
			settingsPage.input.Blur()
			return cmd

		case tea.KeyEsc:
			settingsPage.editing = false
			settingsPage.input.Blur()
			m.message = "Cancelled"
			return nil
		}
	}

	var cmd tea.Cmd
	settingsPage.input, cmd = settingsPage.input.Update(msg)
	return cmd
}

// ===========================================
// View
// ===========================================

// viewSettings renders the settings page.
func (settingsPage *settingsPage) viewSettings(m *model) string {
	var b strings.Builder

	b.WriteString(renderGradient(nameToBanner("Settings")))
	if m.updateAvailable != "" {
		b.WriteString("  " + updateStyle.Render(fmt.Sprintf("A new version: %s is available ⚡ ", m.updateAvailable)))
	}
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(errorStyle.Render(fmt.Sprintf("Error: %v", m.err)))
		b.WriteString("\n")
	}

	config := m.config
	if config == nil {
		config = configutil.DefaultConfig()
	}

	box := listBox
	if m.width > 0 {
		box = box.Width(m.width - 2)
	}

	// Reserve the last scrollbarCols columns of the box content for the
	// scrollbar (a gap column + the bar itself). Rows are wrapped to the
	// remaining contentWidth so a long value can't slip under the bar.
	const scrollbarCols = 2
	innerWidth := 28
	if m.width > 0 {
		innerWidth = max(1, m.width-2-box.GetHorizontalFrameSize())
	}
	contentWidth := max(1, innerWidth-scrollbarCols)

	var listContent strings.Builder
	currentItem := settingsPage.settingsCursorItem(m)

	// itemLineStart maps item ID -> the line index (within listContent)
	// where the item begins. After the scroll offset is known this is
	// converted to an on-screen Y for the visible items so mouse clicks
	// resolve back to a cursor index.
	clear(settingsPage.itemRowYs)
	clear(settingsPage.itemHeights)
	itemLineStart := make(map[int]int)
	recordRow := func(item int, content string) {
		// Constrain the row to the content width; a long value wraps onto
		// continuation lines rather than overflowing the fixed-width box.
		content = lipgloss.NewStyle().Width(contentWidth).Render(content)
		itemLineStart[item] = strings.Count(listContent.String(), "\n")
		settingsPage.itemHeights[item] = 1 + strings.Count(content, "\n")
		listContent.WriteString(content)
	}

	for _, section := range settingsSections {
		if !settingsPage.sectionVisible(m, section) {
			continue
		}

		listContent.WriteString(fleetExpandedStyle.Render(section.Title))
		listContent.WriteString("\n")
		listContent.WriteString(dimStyle.Render(strings.Repeat("─", contentWidth)))
		listContent.WriteString("\n\n")

		switch section.Title {
		case "General":
			vimKeysValue := "[ off ]"
			if config.GeneralSettings.TmuxVimKeysEnabled() {
				vimKeysValue = "[ on ]"
			}
			recordRow(settingsItemTmuxVimKeys, settingsPage.renderSettingsRow(m, currentItem == settingsItemTmuxVimKeys, "Tmux vim keys", vimKeysValue))
			listContent.WriteString("\n")

			helpTextValue := "[ off ]"
			if config.GeneralSettings.ShowHelpTextEnabled() {
				helpTextValue = "[ on ]"
			}
			recordRow(settingsItemShowHelpText, settingsPage.renderSettingsRow(m, currentItem == settingsItemShowHelpText, "Show help text", helpTextValue))

			if m.updateAvailable != "" {
				listContent.WriteString("\n")
				updateValue := updateStyle.Render(m.updateAvailable+" available ⚡") + "  " + dimStyle.Render("press enter to update")
				recordRow(settingsItemUpdate, settingsPage.renderSettingsRow(m, currentItem == settingsItemUpdate, "Update", updateValue))
			}

		case "Dotfiles":
			repoValue := config.DotfilesSettings.RepoURL
			if repoValue == "" && !(settingsPage.editing && currentItem == settingsItemDotfilesRepo) {
				repoValue = dimStyle.Render("(not set)")
			}
			recordRow(settingsItemDotfilesRepo, settingsPage.renderSettingsRow(m, currentItem == settingsItemDotfilesRepo, "Repository URL", repoValue))
			listContent.WriteString("\n")

			scriptValue := config.DotfilesSettings.InstallScript
			if scriptValue == "" && !(settingsPage.editing && currentItem == settingsItemDotfilesScript) {
				scriptValue = dimStyle.Render("(not set)")
			}
			recordRow(settingsItemDotfilesScript, settingsPage.renderSettingsRow(m, currentItem == settingsItemDotfilesScript, "Install script", scriptValue))
			listContent.WriteString("\n")

			autoInstallValue := "[ off ]"
			if config.DotfilesSettings.AutoInstall {
				autoInstallValue = "[ on ]"
			}
			recordRow(settingsItemDotfilesAutoInstall, settingsPage.renderSettingsRow(m, currentItem == settingsItemDotfilesAutoInstall, "Auto install", autoInstallValue))
			listContent.WriteString("\n")

			agentName, _, agentErr := agent.FindAgent()
			var setupValue string
			if agentErr != nil {
				setupValue = statusCreatingStyle.Render("no agent found") + "  " + dimStyle.Render("install claude, codex, gemini, or copilot")
			} else {
				setupValue = statusRunningStyle.Render(agentName) + "  " + dimStyle.Render("press enter to get help setting up dotfiles")
			}
			recordRow(settingsItemDotfilesSetup, settingsPage.renderSettingsRow(m, currentItem == settingsItemDotfilesSetup, "Help me set this up", setupValue))

		case "Coder":
			templateValue := config.CoderSettings.Template
			if templateValue == "" && !(settingsPage.editing && currentItem == settingsItemCoderTemplate) {
				templateValue = dimStyle.Render("(not set)")
			}
			if m.coderFetchingParams {
				templateValue += "  " + m.spinner.View() + " fetching..."
			}
			recordRow(settingsItemCoderTemplate, settingsPage.renderSettingsRow(m, currentItem == settingsItemCoderTemplate, "Template", templateValue))
			listContent.WriteString("\n")

			presetValue := config.CoderSettings.Preset
			if presetValue == "" {
				presetValue = dimStyle.Render("(none)")
			} else {
				presetValue = fmt.Sprintf("[ %s ]", presetValue)
			}
			recordRow(settingsItemCoderPreset, settingsPage.renderSettingsRow(m, currentItem == settingsItemCoderPreset, "Preset", presetValue))

			for i, param := range config.CoderSettings.Parameters {
				listContent.WriteString("\n")
				paramItem := settingsItemCoderParamBase + i
				value := param.Value
				if value == "" && !(settingsPage.editing && currentItem == paramItem) {
					if param.DefaultValue != "" {
						value = dimStyle.Render(param.DefaultValue + " (default)")
					} else {
						value = dimStyle.Render("(not set)")
					}
				}
				label := param.Name
				if param.DisplayName != "" {
					label = param.DisplayName
				}
				recordRow(paramItem, settingsPage.renderSettingsRow(m, currentItem == paramItem, label, value))
			}

		case "Codespaces":
			var machineValue string
			if config.CodespacesSettings.Machine == "" {
				if m.codespaceFetchingMachines {
					machineValue = m.spinner.View() + " fetching..."
				} else {
					machineValue = dimStyle.Render("(none)")
				}
			} else {
				machineValue = fmt.Sprintf("[ %s ]", config.CodespacesSettings.Machine)
				if label := settingsPage.codespacesMachineLabel(m); label != config.CodespacesSettings.Machine {
					machineValue += "\n" + strings.Repeat(" ", 21) + dimStyle.Render(label)
				}
			}
			recordRow(settingsItemCodespacesMachine, settingsPage.renderSettingsRow(m, currentItem == settingsItemCodespacesMachine, "Machine", machineValue))

		case "Browser":
			multipleValue := "[ off ]"
			if config.BrowserSettings.MultipleBrowsersPerFleetEnabled() {
				multipleValue = "[ on ]"
			}
			recordRow(settingsItemBrowserMultiple, settingsPage.renderSettingsRow(m, currentItem == settingsItemBrowserMultiple, "Enable Multiple Browsers Per Fleet", multipleValue))

			if !config.BrowserSettings.MultipleBrowsersPerFleetEnabled() {
				listContent.WriteString("\n")
				autoSwitchValue := "[ off ]"
				if config.BrowserSettings.AutoSwitchEnabled() {
					autoSwitchValue = "[ on ]"
				}
				// Append a dim sub-line under the value so the
				// setting carries its own one-line description.
				// The 21-space indent matches cursor (2) + label
				// (%-18s) + value-separator (1).
				autoSwitchValue += "\n" + strings.Repeat(" ", 21) + dimStyle.Render("Do not prompt when switching the browser to another instance")
				recordRow(settingsItemBrowserAutoSwitch, settingsPage.renderSettingsRow(m, currentItem == settingsItemBrowserAutoSwitch, "Auto Switch", autoSwitchValue))
			}

		case "Fleet MCP":
			// Copy local config — the common task, so it leads the section.
			recordRow(settingsItemRemoteMcpCopyLocal, settingsPage.renderSettingsRow(m, currentItem == settingsItemRemoteMcpCopyLocal, "Copy local MCP config", dimStyle.Render("press enter to copy mcp.json for the loopback server")))

			// Copy remote config — only meaningful once the tunnel is on.
			if config.RemoteMcpSettings.Enabled {
				listContent.WriteString("\n")
				remoteHint := "press enter to copy mcp.json with the public URL"
				if remoteMcpPublicURL(m) == "" {
					remoteHint = "connect to the gateway first to get a public URL"
				}
				recordRow(settingsItemRemoteMcpCopyRemote, settingsPage.renderSettingsRow(m, currentItem == settingsItemRemoteMcpCopyRemote, "Copy remote MCP config", dimStyle.Render(remoteHint)))
			}
			listContent.WriteString("\n")

			enabledValue := "[ off ]"
			if config.RemoteMcpSettings.Enabled {
				enabledValue = "[ on ]"
			}
			// Append a dim sub-line describing what the toggle does.
			enabledValue += "\n" + strings.Repeat(" ", 21) + dimStyle.Render("Expose this fleet's MCP server to the internet via a fleet gateway")
			recordRow(settingsItemRemoteMcpEnabled, settingsPage.renderSettingsRow(m, currentItem == settingsItemRemoteMcpEnabled, "Enable Remote MCP", enabledValue))
			listContent.WriteString("\n")

			fleetEnabledValue := "[ off ]"
			if config.RemoteMcpSettings.FleetEnabled {
				fleetEnabledValue = "[ on ]"
			}
			fleetEnabledValue += "\n" + strings.Repeat(" ", 21) + dimStyle.Render("Allow remote `fleet` binary to control this instance through the gateway public url")
			recordRow(settingsItemRemoteFleetEnabled, settingsPage.renderSettingsRow(m, currentItem == settingsItemRemoteFleetEnabled, "Enable Remote Fleet", fleetEnabledValue))
			listContent.WriteString("\n")

			gatewayValue := config.RemoteMcpSettings.GatewayURL
			if gatewayValue == "" && !(settingsPage.editing && currentItem == settingsItemRemoteMcpGatewayURL) {
				gatewayValue = dimStyle.Render("(not set)")
			}
			recordRow(settingsItemRemoteMcpGatewayURL, settingsPage.renderSettingsRow(m, currentItem == settingsItemRemoteMcpGatewayURL, "Gateway URL", gatewayValue))

			// The Public MCP URL / Public GRPC URL rows show the live tunnel
			// status (state + gateway-assigned address) and are navigable so
			// enter/click copies the raw URL — see updateSettingsNav. Each appears
			// only once its feature is enabled. The Bearer Token row below copies
			// the shared secret (~/.fleet/mcp.token) those URLs authenticate with.
			// Each URL row advertises its copy action inline (matching the copy
			// rows above) only when there's actually a URL to copy; otherwise the
			// status value already explains why (e.g. "(not connected)").
			if config.RemoteMcpSettings.Enabled {
				listContent.WriteString("\n")
				mcpValue := remoteMcpStatusValue(m)
				if remoteMcpPublicURL(m) != "" {
					mcpValue += "  " + dimStyle.Render("press enter to copy")
				}
				recordRow(settingsItemRemoteMcpPublicURL, settingsPage.renderSettingsRow(m, currentItem == settingsItemRemoteMcpPublicURL, "Public MCP URL", mcpValue))
			}
			if config.RemoteMcpSettings.FleetEnabled {
				listContent.WriteString("\n")
				grpcValue := remoteGrpcStatusValue(m)
				if remoteGrpcPublicURL(m) != "" {
					grpcValue += "  " + dimStyle.Render("press enter to copy")
				}
				recordRow(settingsItemRemoteGrpcPublicURL, settingsPage.renderSettingsRow(m, currentItem == settingsItemRemoteGrpcPublicURL, "Public GRPC URL", grpcValue))
			}
			if config.RemoteMcpSettings.Enabled || config.RemoteMcpSettings.FleetEnabled {
				listContent.WriteString("\n")
				tokenValue := "[ Copy Bearer Token ]  " + dimStyle.Render("press enter to copy the daemon bearer token")
				recordRow(settingsItemRemoteMcpToken, settingsPage.renderSettingsRow(m, currentItem == settingsItemRemoteMcpToken, "Bearer Token", tokenValue))
			}

		case "Fleet Armada":
			// One row per registered remote: URL + live ping status +
			// [ delete ] button (cache-clear UX), then the add button.
			for i, remote := range m.armadaRemotes {
				item := settingsItemArmadaBase + i
				active := currentItem == item
				value := remote.URL + "  " + armadaStatusValue(m, remote.URL) + "  " + settingsPage.renderArmadaDeleteButton(m, active)
				recordRow(item, settingsPage.renderSettingsRow(m, active, fmt.Sprintf("Remote %d", i+1), value))
				listContent.WriteString("\n")
			}

			addActive := currentItem == settingsItemArmadaAdd
			var addValue string
			switch {
			case addActive && settingsPage.armadaAddStage == armadaAddURLIn:
				addValue = "URL: " + settingsPage.input.View()
			case addActive && settingsPage.armadaAddStage == armadaAddTokenIn:
				addValue = "Token: " + settingsPage.input.View()
			case addActive && settingsPage.armadaAddStage == armadaAddTesting:
				addValue = m.spinner.View() + " testing connection…"
			default:
				addValue = dimStyle.Render("press enter to register a remote fleet")
			}
			addValue += "\n" + strings.Repeat(" ", 21) + dimStyle.Render("Registered fleets can be switched to from the main page's Armada selector")
			recordRow(settingsItemArmadaAdd, settingsPage.renderSettingsRow(m, addActive, "+ Remote Fleet", addValue))

		case "Tool Status":
			for i, tool := range m.toolStatus {
				if i > 0 {
					listContent.WriteString("\n")
				}
				var badge string
				if tool.Found {
					badge = statusRunningStyle.Render("installed")
				} else {
					badge = statusCreatingStyle.Render("not found")
				}
				value := badge + "  " + dimStyle.Render(tool.Description)
				itemID := settingsItemToolStatusBase + i
				recordRow(itemID, settingsPage.renderSettingsRow(m, currentItem == itemID, tool.Name, value))
			}

		case "Fleet Daemon":
			var daemonValue string
			if m.daemonRestarting {
				daemonValue = m.spinner.View() + " restarting…"
			} else {
				ver := m.serverVersion
				if ver == "" {
					ver = "dev build"
				}
				daemonValue = statusRunningStyle.Render(ver) + "  " + dimStyle.Render("press enter to relaunch fleetd from this TUI's binary")
			}
			recordRow(settingsItemDaemonRestart, settingsPage.renderSettingsRow(m, currentItem == settingsItemDaemonRestart, "Restart daemon", daemonValue))

		case "Help":
			agentName, _, agentErr := doctor.FindAgent()
			var value string
			if agentErr != nil {
				value = statusCreatingStyle.Render("no agent found") + "  " + dimStyle.Render("install claude, codex, gemini, or copilot")
			} else {
				value = statusRunningStyle.Render(agentName) + "  " + dimStyle.Render("press enter to diagnose your setup")
			}
			recordRow(settingsItemDoctor, settingsPage.renderSettingsRow(m, currentItem == settingsItemDoctor, "Run Doctor", value))
			listContent.WriteString("\n")
			recordRow(settingsItemKeybindings, settingsPage.renderSettingsRow(m, currentItem == settingsItemKeybindings, "Keybindings", dimStyle.Render("press enter to view all keybindings")))
		}

		listContent.WriteString("\n\n")
	}

	// Assemble everything that renders below the box first, so its height
	// can be subtracted when sizing the scrolling viewport.
	var tail strings.Builder
	if settingsPage.showKeybindings {
		tail.WriteString("\n")
		tail.WriteString(keybindingsDialogBox.Render(settingsPage.renderKeybindingsDialog()))
		tail.WriteString("\n")
	}
	// Only the coder PARAMETER rows (value fields, which can interpolate the
	// variables below) get this hint. Coder params occupy
	// [settingsItemCoderParamBase, settingsItemCodespacesMachine); the upper
	// bound must be settingsItemCodespacesMachine, not settingsItemToolStatusBase,
	// or the hint also (wrongly) shows on the codespaces/browser/remote-mcp rows
	// that sit in the 500/600/700 blocks below it.
	if currentItem >= settingsItemCoderParamBase && currentItem < settingsItemCodespacesMachine {
		tail.WriteString(dimStyle.Render("  Variables: ${GIT_URL} = fleet repo URL, ${GIT_BRANCH} = git branch (blank = default), ${INSTANCE_NAME} = workspace name"))
		tail.WriteString("\n")
	}
	if isArmadaRemoteItem(currentItem) && settingsPage.armadaAddStage == armadaAddNone {
		tail.WriteString(dimStyle.Render("  enter: ping now  right/l: focus [ delete ]  enter twice on [ delete ]: remove"))
		tail.WriteString("\n")
	}
	// Copy rows act on enter (not edit/cycle), so spell that out — the generic
	// footer's "enter: edit / left/right: cycle" doesn't apply to them.
	if isCopyRow(currentItem) {
		tail.WriteString(dimStyle.Render("  enter: copy to clipboard"))
		tail.WriteString("\n")
	}
	if m.message != "" {
		tail.WriteString(messageStyle.Render(m.message))
		tail.WriteString("\n")
	}
	if settingsPage.armadaAddStage == armadaAddURLIn || settingsPage.armadaAddStage == armadaAddTokenIn {
		tail.WriteString(renderHelp(m.width, []string{
			"enter: next", "esc: cancel",
		}))
	} else if settingsPage.editing {
		tail.WriteString(renderHelp(m.width, []string{
			"enter: save", "esc: cancel",
		}))
	} else {
		tail.WriteString(renderHelp(m.width, []string{
			"j/k: navigate", "left/right: cycle", "enter: edit", "esc: back", "ctrl+c: quit",
		}))
	}

	// The box adds a top border before its content, hence +1.
	firstContentY := strings.Count(b.String(), "\n") + 1
	lines := strings.Split(strings.TrimRight(listContent.String(), "\n"), "\n")
	totalLines := len(lines)

	// Size the viewport to whatever vertical space is left after the head,
	// the box borders, and the tail. With no known height (e.g. tests) the
	// whole list is shown.
	viewHeight := totalLines
	if m.height > 0 {
		head := strings.Count(b.String(), "\n")
		avail := m.height - head - 2 - lipgloss.Height(tail.String())
		viewHeight = max(3, avail)
	}
	if viewHeight > totalLines {
		viewHeight = totalLines
	}

	// Chase the selection only when it moved (keyboard nav or a click); a
	// plain re-render after a wheel scroll leaves the viewport where it is.
	offset := settingsPage.scrollOffset
	if settingsPage.cursor != settingsPage.lastViewCursor {
		if start, ok := itemLineStart[currentItem]; ok {
			end := start + settingsPage.itemHeights[currentItem] - 1
			if start < offset {
				offset = start
			}
			if end > offset+viewHeight-1 {
				offset = end - viewHeight + 1
			}
		}
	}
	settingsPage.lastViewCursor = settingsPage.cursor
	offset = max(0, min(offset, totalLines-viewHeight))
	settingsPage.scrollOffset = offset

	// Map the visible items to on-screen Y for mouse hit-testing.
	visibleEnd := offset + viewHeight
	for item, start := range itemLineStart {
		if start >= offset && start < visibleEnd {
			settingsPage.itemRowYs[item] = firstContentY + (start - offset)
		}
	}

	visible := strings.Join(lines[offset:visibleEnd], "\n")
	if totalLines > viewHeight {
		// Pad the content to a stable width and lay the scrollbar down its
		// right edge.
		content := lipgloss.NewStyle().Width(contentWidth).Render(visible)
		bar := renderScrollbar(viewHeight, totalLines, offset)
		visible = lipgloss.JoinHorizontal(lipgloss.Top, content, " ", bar)
	}

	b.WriteString(box.Render(visible))
	b.WriteString("\n")
	b.WriteString(tail.String())

	return b.String()
}

// renderScrollbar draws a vertical scrollbar viewHeight rows tall for a list
// of total lines currently scrolled to offset. The first and last rows are
// up/down arrows; the thumb between them is sized and positioned to reflect
// the visible fraction.
func renderScrollbar(viewHeight, total, offset int) string {
	track := viewHeight
	arrow := false
	if viewHeight >= 3 {
		track = viewHeight - 2 // reserve a row for each arrow
		arrow = true
	}

	thumb := min(max(1, track*viewHeight/total), track)
	thumbPos := 0
	if maxOffset := total - viewHeight; maxOffset > 0 {
		thumbPos = offset * (track - thumb) / maxOffset
	}

	var b strings.Builder
	for i := range viewHeight {
		if i > 0 {
			b.WriteString("\n")
		}
		switch {
		case arrow && i == 0:
			b.WriteString(scrollbarArrowStyle.Render("▲"))
		case arrow && i == viewHeight-1:
			b.WriteString(scrollbarArrowStyle.Render("▼"))
		default:
			row := i
			if arrow {
				row = i - 1
			}
			if row >= thumbPos && row < thumbPos+thumb {
				b.WriteString(scrollbarThumbStyle.Render("█"))
			} else {
				b.WriteString(scrollbarTrackStyle.Render("░"))
			}
		}
	}
	return b.String()
}

// renderSettingsRow renders a single settings row with optional cursor
// and editing state.
func (settingsPage *settingsPage) renderSettingsRow(m *model, active bool, label string, value string) string {
	cursor := "  "
	if active {
		cursor = cursorStyle.Render("> ")
	}

	formattedLabel := fmt.Sprintf("%-18s", label)

	if settingsPage.editing && active {
		value = settingsPage.input.View()
	}

	if active {
		return fmt.Sprintf("%s%s %s", cursor, selectedStyle.Render(formattedLabel), value)
	}
	return fmt.Sprintf("%s%s %s", cursor, formattedLabel, value)
}

// ===========================================
// URL Helper
// ===========================================

// openURL opens the given URL in the user's default browser.
func openURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
