package tui

import (
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// armada.go is the TUI side of the Fleet Armada feature: the registry of
// remote fleetd connections (settings page section), per-remote connection
// status pings, and the live switch of the TUI's active connection between
// "local" and a registered remote (the border selector on the main page).
//
// Persistence always targets the LOCAL daemon (armada_client.go); the switch
// itself works by swapping the FLEET_GATEWAY/FLEET_TOKEN env vars — every
// fleetclient.Dial re-reads them, and spawned `fleet shell` children inherit
// them — then bouncing each connection the TUI holds.

// ===========================================
// Connection status
// ===========================================

// armadaConnState is the lifecycle of one remote's status indicator.
type armadaConnState int

const (
	armadaStatusUnknown armadaConnState = iota
	armadaStatusPinging
	armadaStatusConnected
	armadaStatusError
)

// armadaStatus is the latest ping outcome for one registered remote, keyed by
// URL in m.armadaStatus.
type armadaStatus struct {
	state armadaConnState
	err   string // human-readable cause, set when state == armadaStatusError
}

// armadaPingInterval is the re-ping cadence while the settings page is open
// (the page shows live status indicators; nothing pings in the background
// otherwise).
const armadaPingInterval = 10 * time.Second

// ===========================================
// Messages
// ===========================================

// armadaLoadedMsg delivers the registry from the local daemon.
type armadaLoadedMsg struct {
	remotes []configutil.ArmadaRemote
	err     error
}

// armadaPingResultMsg delivers one remote's ping outcome.
type armadaPingResultMsg struct {
	url string
	err error
}

// armadaPingTickMsg re-pings all remotes while the settings page is open.
type armadaPingTickMsg struct{}

// armadaTestResultMsg delivers the registration connection test outcome for
// the add flow ("+ Remote Fleet"). On success the remote is saved.
type armadaTestResultMsg struct {
	url   string
	token string
	err   error
}

// armadaSaveResultMsg delivers the outcome of persisting the edited registry
// (add or delete). remotes is the saved list (post server normalization).
type armadaSaveResultMsg struct {
	remotes    []configutil.ArmadaRemote
	action     string // "added" / "removed", for the status message
	removedIdx int    // index the delete removed; -1 for adds (cursor re-pin)
	err        error
}

// armadaSwitchedMsg delivers the post-switch state/config reload.
type armadaSwitchedMsg struct {
	label  string
	st     *configutil.State
	config *configutil.Config
	err    error
}

// ===========================================
// Commands
// ===========================================

// fetchArmadaCmd loads the registry from the local daemon.
func fetchArmadaCmd() tea.Cmd {
	return func() tea.Msg {
		remotes, err := fetchArmadaLocal()
		return armadaLoadedMsg{remotes: remotes, err: err}
	}
}

// pingArmadaCmd probes one remote.
func pingArmadaCmd(url, token string) tea.Cmd {
	return func() tea.Msg {
		return armadaPingResultMsg{url: url, err: pingArmadaRemote(url, token)}
	}
}

// armadaPingTickCmd schedules the next status sweep.
func armadaPingTickCmd() tea.Cmd {
	return tea.Tick(armadaPingInterval, func(time.Time) tea.Msg { return armadaPingTickMsg{} })
}

// testArmadaRemoteCmd runs the registration connection test.
func testArmadaRemoteCmd(url, token string) tea.Cmd {
	return func() tea.Msg {
		return armadaTestResultMsg{url: url, token: token, err: pingArmadaRemote(url, token)}
	}
}

// saveArmadaCmd persists the edited registry to the local daemon.
func saveArmadaCmd(remotes []configutil.ArmadaRemote, action string, removedIdx int) tea.Cmd {
	return func() tea.Msg {
		if err := saveArmadaLocal(remotes); err != nil {
			return armadaSaveResultMsg{action: action, removedIdx: removedIdx, err: err}
		}
		return armadaSaveResultMsg{remotes: remotes, action: action, removedIdx: removedIdx}
	}
}

// switchReloadCmd refetches state + config over the NEW endpoint after a
// switch. Runs in a tea.Cmd goroutine: dialing an unreachable remote takes
// seconds and must not stall the Update loop.
func switchReloadCmd(label string) tea.Cmd {
	return func() tea.Msg {
		st, err := fetchStateLegacy()
		if err != nil {
			return armadaSwitchedMsg{label: label, err: err}
		}
		config, err := fetchConfigLegacy()
		if err != nil {
			return armadaSwitchedMsg{label: label, err: err}
		}
		return armadaSwitchedMsg{label: label, st: st, config: config}
	}
}

// ===========================================
// Central message handling (model.Update step 3)
// ===========================================

// handleArmadaMsg processes every armada message. Lives on the model (not the
// settings page) because the registry and statuses outlive page switches; the
// settings page's add/delete flow state is updated when that page is current.
func (m *model) handleArmadaMsg(msg tea.Msg) tea.Cmd {
	settingsPage, _ := m.currentPage.(*settingsPage)

	switch msg := msg.(type) {
	case armadaLoadedMsg:
		if msg.err != nil {
			// The local daemon should always be reachable (it auto-spawns); a
			// failure here is worth surfacing only where the registry is shown.
			if settingsPage != nil {
				m.message = fmt.Sprintf("Failed to load remote fleets: %v", msg.err)
			}
			return nil
		}
		m.armadaRemotes = msg.remotes
		return m.pingAllArmadaCmd()

	case armadaPingTickMsg:
		if settingsPage == nil {
			// Left the settings page — let the tick loop die.
			m.armadaTickArmed = false
			return nil
		}
		return tea.Batch(armadaPingTickCmd(), m.pingAllArmadaCmd())

	case armadaPingResultMsg:
		st := armadaStatus{state: armadaStatusConnected}
		if msg.err != nil {
			st = armadaStatus{state: armadaStatusError, err: armadaPingErrText(msg.err)}
		}
		m.armadaStatus[msg.url] = st
		return nil

	case armadaTestResultMsg:
		if settingsPage == nil || settingsPage.armadaAddStage != armadaAddTesting {
			return nil // flow cancelled or page left; drop the stale result
		}
		if msg.err != nil {
			settingsPage.cancelArmadaAdd()
			m.message = fmt.Sprintf("Connection test failed: %s", armadaPingErrText(msg.err))
			return nil
		}
		m.armadaStatus[msg.url] = armadaStatus{state: armadaStatusConnected}
		next := append(slices.Clone(m.armadaRemotes), configutil.ArmadaRemote{URL: msg.url, Token: msg.token})
		return saveArmadaCmd(next, "added", -1)

	case armadaSaveResultMsg:
		if settingsPage != nil {
			settingsPage.cancelArmadaAdd()
			settingsPage.armadaBusy = false
			settingsPage.armadaDeleteFocused = false
			settingsPage.armadaDeleteConfirm = false
		}
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to save remote fleets: %v", msg.err)
			return nil
		}
		m.armadaRemotes = msg.remotes
		if settingsPage != nil {
			// The list length changed under the cursor; re-pin it sensibly.
			if msg.removedIdx >= 0 && len(msg.remotes) > 0 {
				settingsPage.cursorToItem(m, settingsItemArmadaBase+min(msg.removedIdx, len(msg.remotes)-1))
			} else {
				settingsPage.cursorToItem(m, settingsItemArmadaAdd)
			}
		}
		m.message = "Remote fleet " + msg.action
		return nil

	case armadaSwitchedMsg:
		if msg.err != nil {
			// The new endpoint isn't answering (yet). Keep the error visible;
			// the Watch stream keeps retrying and pushes state when it lands,
			// and the user can switch back via the selector at any time.
			m.err = msg.err
			m.message = fmt.Sprintf("Switched to %s — not reachable yet: %v", msg.label, msg.err)
			return nil
		}
		m.st = msg.st
		m.config = msg.config
		m.err = nil
		m.hydrateSavedGroups()
		m.pruneOrphanedSavedGroups()
		if m.fleetPage != nil {
			m.fleetPage.buildRows(m)
		}
		m.message = fmt.Sprintf("Switched to %s", msg.label)
		return nil
	}
	return nil
}

// pingAllArmadaCmd marks every registered remote as pinging and probes them
// concurrently. Remotes already mid-ping are skipped so overlapping sweeps
// (tick + manual) don't double-probe.
func (m *model) pingAllArmadaCmd() tea.Cmd {
	var cmds []tea.Cmd
	for _, r := range m.armadaRemotes {
		if m.armadaStatus[r.URL].state == armadaStatusPinging {
			continue
		}
		m.armadaStatus[r.URL] = armadaStatus{state: armadaStatusPinging}
		cmds = append(cmds, pingArmadaCmd(r.URL, r.Token))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// armadaPingErrText folds a ping error into a short human cause. The gRPC code
// distinguishes where the chain broke (see fleetclient.Ping).
func armadaPingErrText(err error) string {
	switch status.Code(err) {
	case codes.Unauthenticated:
		return "invalid token"
	case codes.NotFound:
		return "unknown session — daemon offline or Remote Fleet disabled"
	case codes.Unavailable:
		return "gateway unreachable"
	case codes.DeadlineExceeded:
		return "timed out"
	default:
		return err.Error()
	}
}

// ===========================================
// Armada selector entries + live switch
// ===========================================

// armadaEntry is one row of the main-page Armada dropdown.
type armadaEntry struct {
	label   string
	url     string // "" = local
	token   string
	current bool
}

// armadaEntries builds the dropdown: local first, then every registered
// remote, then — when the TUI was booted with FLEET_GATEWAY pointing at an
// unregistered remote — that boot remote, so the current selection is always
// in the list.
func (m *model) armadaEntries() []armadaEntry {
	current := armadaCurrentURL()
	entries := []armadaEntry{{label: "local", current: current == ""}}
	bootSeen := false
	for _, r := range m.armadaRemotes {
		if r.URL == m.bootGateway {
			bootSeen = true
		}
		entries = append(entries, armadaEntry{label: r.URL, url: r.URL, token: r.Token, current: r.URL == current})
	}
	if m.bootGateway != "" && !bootSeen {
		entries = append(entries, armadaEntry{label: m.bootGateway + " (env)", url: m.bootGateway, token: m.bootToken, current: m.bootGateway == current})
	}
	return entries
}

// armadaCurrentURL is the gateway URL of the active connection ("" = local).
// Read from the env because the env IS the switch mechanism — it stays
// correct whether the connection came from boot or a runtime switch.
func armadaCurrentURL() string {
	return os.Getenv("FLEET_GATEWAY")
}

// armadaCurrentLabel names the active connection for the border selector.
func armadaCurrentLabel() string {
	if gw := os.Getenv("FLEET_GATEWAY"); gw != "" {
		return gw
	}
	if srv := os.Getenv("FLEET_SERVER"); srv != "" {
		return srv
	}
	return "local"
}

// switchArmada retargets the TUI onto entry's daemon: tear down everything
// bound to the old connection, swap the env vars every dial path re-reads,
// bounce the Watch stream, blank the daemon-derived caches, and kick off the
// async state/config reload. The Watch reconnect (IncludeInitialState) and
// the reload both repopulate the view.
func (m *model) switchArmada(entry armadaEntry) tea.Cmd {
	fleetPage := m.fleetPage

	// 1. Split panes run child processes attached to the old daemon.
	if fleetPage != nil && fleetPage.splitPaneID != "" {
		killAllSplitPanes()
		unbindHostSplitKeys()
		fleetPage.clearSplit()
	}

	// 2. Port-forward listeners (and browser proxies riding them) hold dialer
	// closures over the old connection.
	m.portForwards.Shutdown()
	m.activeBrowser = make(map[string]string)

	// 3. Drop the cached mutation conn; the next RPC re-dials with the new env.
	closeMutationConn()

	// 4. The env vars are the single switch point: every fleetclient.Dial
	// re-reads them, IsRemote() follows, and `fleet shell` children spawned
	// from here on inherit them.
	if entry.url == "" {
		_ = os.Unsetenv("FLEET_GATEWAY")
		_ = os.Unsetenv("FLEET_SERVER")
		_ = os.Unsetenv("FLEET_TOKEN")
	} else {
		_ = os.Setenv("FLEET_GATEWAY", entry.url)
		_ = os.Setenv("FLEET_TOKEN", entry.token)
		_ = os.Unsetenv("FLEET_SERVER")
	}

	// 5. Reconnect the Watch stream to the new endpoint (re-arms the
	// once-per-connection FleetTUIConnected nudge).
	bounceWatchStream()

	// 6. Blank every daemon-derived cache so nothing from the old fleet
	// lingers (m.runtime merges by key and would otherwise keep stale rows).
	m.st = &configutil.State{}
	m.pstate = nil
	clear(m.runtime)
	clear(m.creating)
	m.remoteMcpStatus = nil
	m.sessionStore = NewSessionStore()
	if fleetPage != nil {
		fleetPage.savedGroups = make(map[string]savedGroup)
		fleetPage.collapsed = make(map[string]bool)
		fleetPage.cursor = 0
		fleetPage.buildRows(m)
	}

	m.message = fmt.Sprintf("Switching to %s…", entry.label)
	return switchReloadCmd(entry.label)
}
