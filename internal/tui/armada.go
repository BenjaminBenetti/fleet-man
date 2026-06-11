package tui

import (
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
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

// armadaSwitchedMsg delivers the post-switch state/config reload. gen is the
// connection generation the reload was started for, so a late reply from an
// abandoned switch can be discarded.
type armadaSwitchedMsg struct {
	label  string
	gen    int
	st     *configutil.State
	config *configutil.Config
	err    error
}

// armadaConfigLoadedMsg delivers a config refetch triggered when the new
// daemon's first Watch state arrives after a switch whose synchronous reload
// had failed (slow endpoint).
type armadaConfigLoadedMsg struct {
	gen    int
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
// seconds and must not stall the Update loop. gen tags the result so a reply
// from a superseded switch is dropped.
func switchReloadCmd(label string, gen int) tea.Cmd {
	return func() tea.Msg {
		st, err := fetchStateLegacy()
		if err != nil {
			return armadaSwitchedMsg{label: label, gen: gen, err: err}
		}
		config, err := fetchConfigLegacy()
		if err != nil {
			return armadaSwitchedMsg{label: label, gen: gen, err: err}
		}
		return armadaSwitchedMsg{label: label, gen: gen, st: st, config: config}
	}
}

// reloadConfigCmd refetches just the config over the current endpoint, gen
// tagged so a reply from a superseded connection is dropped.
func reloadConfigCmd(gen int) tea.Cmd {
	return func() tea.Msg {
		config, err := fetchConfigLegacy()
		return armadaConfigLoadedMsg{gen: gen, config: config, err: err}
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
		// A switch made after this reload was dispatched bumped the generation;
		// a late reply from the abandoned switch must not clobber the current
		// connection's state.
		if msg.gen != m.watchGen {
			return nil
		}
		if msg.err != nil {
			// The new endpoint isn't answering yet (slow remote, or a local
			// auto-spawn still coming up). Don't set the sticky m.err banner —
			// the bounced Watch stream keeps retrying and pushes IncludeInitial
			// state when it lands, and the user can switch back at any time.
			m.message = fmt.Sprintf("Switched to %s — waiting for it to come online…", msg.label)
			return nil
		}
		m.st = msg.st
		m.config = msg.config
		m.armadaConfigPending = false
		m.err = nil
		m.resumeCreatingFromState()
		m.hydrateSavedGroups()
		m.pruneOrphanedSavedGroups()
		if m.fleetPage != nil {
			m.fleetPage.buildRows(m)
		}
		m.message = fmt.Sprintf("Switched to %s", msg.label)
		return m.postSwitchFetchCmd()

	case armadaConfigLoadedMsg:
		// Late config refetch after a slow switch (see armadaConfigPending).
		if msg.gen != m.watchGen || msg.err != nil || msg.config == nil {
			return nil
		}
		m.config = msg.config
		return m.postSwitchFetchCmd()
	}
	return nil
}

// resumeCreatingFromState repopulates m.creating from the freshly fetched
// state, mirroring newModel's startup logic so the new daemon's in-progress
// instances are tracked to completion.
func (m *model) resumeCreatingFromState() {
	if m.st == nil {
		return
	}
	for fleetName, f := range m.st.Fleets {
		for _, instance := range f.Instances {
			if instance.Status == fleet.StatusCreating || instance.Status == fleet.StatusCloning {
				m.creating[fleetName+"/"+instance.Name] = true
			}
		}
	}
}

// postSwitchFetchCmd re-runs the daemon-derived auto-fetches a fresh boot would
// (Coder template params, codespace machine types) plus the creating-poll, so
// the new daemon's config drives the settings page instead of the old one's
// stale lists.
func (m *model) postSwitchFetchCmd() tea.Cmd {
	var cmds []tea.Cmd
	if len(m.creating) > 0 {
		cmds = append(cmds, pollCreatingCmd())
	}
	if m.config != nil && m.config.CoderSettings.Template != "" {
		m.coderFetchingParams = true
		cmds = append(cmds, fetchCoderParamsCmd(m.config.CoderSettings.Template))
	}
	if repo := m.firstFleetRepo(); repo != "" {
		m.codespaceFetchingMachines = true
		cmds = append(cmds, fetchCodespaceMachinesCmd(repo))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// pingAllArmadaCmd marks every pingable remote (registered remotes plus an
// unregistered gateway boot remote) as pinging and probes them concurrently.
// Remotes already mid-ping are skipped so overlapping sweeps (tick + manual)
// don't double-probe.
func (m *model) pingAllArmadaCmd() tea.Cmd {
	type target struct{ url, token string }
	var targets []target
	for _, r := range m.armadaRemotes {
		targets = append(targets, target{r.URL, r.Token})
	}
	// The gateway boot remote shows in the dropdown as "(env)" even when not
	// registered, so give it a status too.
	if m.bootGateway != "" {
		registered := false
		for _, r := range m.armadaRemotes {
			if r.URL == m.bootGateway {
				registered = true
				break
			}
		}
		if !registered {
			targets = append(targets, target{m.bootGateway, m.bootToken})
		}
	}

	var cmds []tea.Cmd
	for _, t := range targets {
		if m.armadaStatus[t.url].state == armadaStatusPinging {
			continue
		}
		m.armadaStatus[t.url] = armadaStatus{state: armadaStatusPinging}
		cmds = append(cmds, pingArmadaCmd(t.url, t.token))
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

// armadaEntry is one row of the main-page Armada dropdown. Exactly one of url
// (a gateway) / server (a plain FLEET_SERVER target) is set for a remote; both
// empty means local.
type armadaEntry struct {
	label   string
	url     string // gateway URL ("" unless a gateway entry)
	token   string
	server  string // FLEET_SERVER host:port ("" unless a plain-TCP entry)
	current bool
}

// key uniquely identifies the endpoint an entry points at, matching
// armadaCurrentKey()'s encoding so "is this the active connection?" is a
// string compare.
func (e armadaEntry) key() string {
	switch {
	case e.url != "":
		return e.url
	case e.server != "":
		return "server:" + e.server
	default:
		return ""
	}
}

// armadaEntries builds the dropdown: local first, then every registered
// remote, then — when the TUI was booted pointing at an UNREGISTERED remote
// (FLEET_GATEWAY or FLEET_SERVER) — that boot remote, so the current selection
// is always present and selectable.
func (m *model) armadaEntries() []armadaEntry {
	current := armadaCurrentKey()
	entries := []armadaEntry{{label: "local"}}
	bootGatewaySeen := false
	for _, r := range m.armadaRemotes {
		if r.URL == m.bootGateway {
			bootGatewaySeen = true
		}
		entries = append(entries, armadaEntry{label: r.URL, url: r.URL, token: r.Token})
	}
	if m.bootGateway != "" && !bootGatewaySeen {
		entries = append(entries, armadaEntry{label: m.bootGateway + " (env)", url: m.bootGateway, token: m.bootToken})
	}
	if m.bootServer != "" {
		// FLEET_SERVER targets aren't registrable (no token / gateway), so the
		// boot value is the only way to represent or return to one.
		entries = append(entries, armadaEntry{label: m.bootServer + " (env)", server: m.bootServer})
	}
	for i := range entries {
		entries[i].current = entries[i].key() == current
	}
	return entries
}

// armadaCurrentKey identifies the active connection from the live env (the env
// IS the switch mechanism, so it's correct whether the connection came from
// boot or a runtime switch). "" = local.
func armadaCurrentKey() string {
	if gw := os.Getenv("FLEET_GATEWAY"); gw != "" {
		return gw
	}
	if srv := os.Getenv("FLEET_SERVER"); srv != "" {
		return "server:" + srv
	}
	return ""
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

	// 1. The env vars are the single switch point: every fleetclient.Dial
	// re-reads them, IsRemote() follows, and `fleet shell` children spawned
	// from here on inherit them. Swap them FIRST so any RPC that re-dials
	// during the teardown below targets the NEW endpoint, never the old one.
	switch {
	case entry.url != "":
		_ = os.Setenv("FLEET_GATEWAY", entry.url)
		_ = os.Setenv("FLEET_TOKEN", entry.token)
		_ = os.Unsetenv("FLEET_SERVER")
	case entry.server != "":
		_ = os.Setenv("FLEET_SERVER", entry.server)
		_ = os.Unsetenv("FLEET_GATEWAY")
		_ = os.Unsetenv("FLEET_TOKEN")
	default: // local
		_ = os.Unsetenv("FLEET_GATEWAY")
		_ = os.Unsetenv("FLEET_SERVER")
		_ = os.Unsetenv("FLEET_TOKEN")
	}

	// 2. Split panes run child processes attached to the old daemon. Bump the
	// restore sequence so an in-flight group restore's splitPaneMsg is rejected
	// (it would otherwise bind a pane to the old fleet and persist its layout).
	if fleetPage != nil {
		fleetPage.restoreSeq++
		if fleetPage.splitPaneID != "" {
			killAllSplitPanes()
			unbindHostSplitKeys()
			fleetPage.clearSplit()
		}
	}

	// 3. Port-forward listeners (and browser proxies riding them) hold dialer
	// closures over the old connection.
	m.portForwards.Shutdown()
	m.activeBrowser = make(map[string]string)

	// 4. Drop the cached mutation conn; the next RPC re-dials with the new env
	// (set in step 1). dialMutation no longer holds its lock across Dial, so
	// this can't freeze the Update loop behind a slow in-flight dial.
	closeMutationConn()

	// 5. Reconnect the Watch stream to the new endpoint and adopt the new
	// connection generation, so events still in flight from the OLD stream are
	// dropped instead of repainting the just-switched caches.
	m.watchGen = bounceWatchStream()

	// 6. Blank every daemon-derived cache so nothing from the old fleet lingers
	// (m.runtime merges by key and would otherwise keep stale rows; m.config
	// must not leak the old daemon's settings into the new daemon's settings
	// page / saves). The reload (or the Watch initial state) repopulates them.
	m.st = &configutil.State{}
	m.pstate = nil
	m.config = configutil.DefaultConfig()
	m.armadaConfigPending = true
	clear(m.runtime)
	clear(m.creating)
	m.remoteMcpStatus = nil
	m.coderPresets = nil
	m.codespaceMachines = nil
	m.coderFetchingParams = false
	m.codespaceFetchingMachines = false
	m.sessionStore = NewSessionStore()
	m.err = nil
	if fleetPage != nil {
		fleetPage.savedGroups = make(map[string]savedGroup)
		fleetPage.collapsed = make(map[string]bool)
		fleetPage.cursor = 0
		fleetPage.buildRows(m)
	}

	m.message = fmt.Sprintf("Switching to %s…", entry.label)
	return switchReloadCmd(entry.label, m.watchGen)
}
