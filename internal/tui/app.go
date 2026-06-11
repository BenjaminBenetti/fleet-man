package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/admiralmcp"
	"github.com/BenjaminBenetti/fleet-man/internal/admiralskill"
	"github.com/BenjaminBenetti/fleet-man/internal/codespaceerr"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/portforward"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ===========================================
// Model
// ===========================================

type model struct {
	st     *configutil.State
	config *configutil.Config
	err    error

	// pstate is the persisted snapshot pushed by the server's Watch stream;
	// runtime is the live sidecar (agent activity/stats/sessions/...) keyed by
	// "fleet/instance". The fleet-list rows render agent activity + CPU/mem
	// stats from runtime — the server owns those pollers now.
	pstate  *fleetgrpc.State
	runtime map[string]*fleetgrpc.InstanceRuntime

	// remoteMcpStatus is the latest outbound-MCP-gateway tunnel status pushed by
	// the server over Watch (connection state + the computed Public MCP URL). The
	// settings page renders it read-only; nil means "no status received yet".
	remoteMcpStatus *fleetgrpc.RemoteMcpStatus

	// Page routing
	currentPage Page
	fleetPage   *fleetPage // persistent — has running state accessed by background message handlers

	spinner      spinner.Model
	agentSpinner spinner.Model   // pulse throbber rendered next to running agents' instance names
	creating     map[string]bool // "fleet/instance" keys currently being created

	coderPresets        []string // available preset names (in-memory, from API)
	coderFetchingParams bool     // true while fetching template parameters

	codespaceMachines         []codespaceMachine // available machine types (from GitHub API)
	codespaceFetchingMachines bool               // true while fetching machine types

	toolStatus []deps.ToolStatus // cached tool install statuses for settings page

	// Port forwarding
	portForwards *portforward.Manager // manages active port forward processes

	// activeBrowser records which instance the browser bound to a given
	// Chrome data dir is currently proxied to: data dir → "<fleet>/<instance>".
	// A data dir is shared across a fleet's instances (unless
	// MultipleBrowsersPerFleet), and a Chrome process can only be proxied to one
	// instance at a time, so a control-socket "open" for a different instance
	// must switch the browser over rather than forward the URL into the
	// wrong-proxied existing process. Populated on every successful browser open.
	activeBrowser map[string]string

	// Per-instance session state: discovery, expansion, last-active
	// session. All three are owned by the SessionStore so every read
	// or write is forced through an InstanceRef and cannot collide
	// across instances that share a session or group name.
	sessionStore *SessionStore

	// Split pane mode: when fleet runs inside a host tmux session,
	// pressing enter opens the instance shell in a right-side pane
	// instead of suspending the TUI.
	inHostTmux bool // true when TMUX env var is set at startup

	// Update check
	updateAvailable string // non-empty = new version tag from GitHub

	// Release notes overlay: shown once after an upgrade. When
	// releaseNotesVersion is non-empty the dialog is active and swallows
	// all key input until dismissed. See releasenotes.go.
	releaseNotesVersion string
	releaseNotesBody    string

	// Pending exec after quit: after a successful update the TUI
	// quits, then Run() replaces the current process with the new
	// fleet binary via syscall.Exec so the new fleet is NOT nested
	// inside the old fleet process.
	pendingExecPath string
	pendingExecArgs []string

	message  string
	quitting bool
	width    int
	height   int
}

// needsDepsCheck reports whether the first-startup dependency check should
// run. "First startup" = the ~/.fleet/ dir doesn't exist. The check is a
// local-daemon concern — deps like docker/devcontainer/tmux only matter on
// the machine where fleetd runs — so it is skipped entirely for a remote
// endpoint (FLEET_GATEWAY / FLEET_SERVER), where no local daemon is spawned
// and ~/.fleet/ never appears.
func needsDepsCheck() bool {
	if fleetclient.IsRemote() {
		return false
	}
	_, err := os.Stat(fleetpaths.Dir())
	return os.IsNotExist(err)
}

// newModel creates and initialises the top-level model, including all
// page instances and their initial state.
func newModel() model {
	spinnerModel := spinner.New()
	spinnerModel.Spinner = spinner.Dot
	spinnerModel.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("170"))

	agentSpinnerModel := spinner.New()
	agentSpinnerModel.Spinner = spinner.Spinner{
		Frames: []string{"·", "✦", "✻", "✶", "✻", "✦"},
		FPS:    time.Second / 4,
	}
	agentSpinnerModel.Style = agentWorkingStyle

	m := model{
		creating:      make(map[string]bool),
		runtime:       make(map[string]*fleetgrpc.InstanceRuntime),
		portForwards:  portforward.NewManager(),
		activeBrowser: make(map[string]string),
		sessionStore:  NewSessionStore(),
		spinner:       spinnerModel,
		agentSpinner:  agentSpinnerModel,
		inHostTmux:    os.Getenv("TMUX") != "",
	}

	// Create the fleet page (persistent — background handlers reference it)
	m.fleetPage = newFleetPage()
	m.currentPage = m.fleetPage

	// Unbind C-PPage/C-NPage from the host tmux so they pass through
	// to inner tmux sessions for session cycling. Bind Ctrl+Q/O to
	// close all split panes from any pane.
	if m.inHostTmux {
		bindHostSessionCycleKeys()
		bindHostCloseKeys()
		bindRefocusTUIKey()
		// Neutralise default split bindings so the user doesn't
		// accidentally open a host shell before selecting an instance.
		// These will be rebound to connect to the active instance
		// once a split pane is opened.
		unbindDefaultSplitKeys()
	}
	// On first-ever startup, check for required binaries and show results
	// if anything is missing.
	if needsDepsCheck() {
		result := deps.Check()
		if deps.HasMissing(result) {
			m.currentPage = newDepsCheckPage(result)
		}
	}

	m.reload()

	// Rehydrate saved pane layouts from disk so group restores after
	// a fleet restart use the exact geometry the user left behind,
	// then drop any layouts whose instance no longer exists.
	m.hydrateSavedGroups()
	m.pruneOrphanedSavedGroups()

	// Resume tracking any instances still in "creating" state from a previous session
	if m.st != nil {
		for fleetName, f := range m.st.Fleets {
			for _, instance := range f.Instances {
				if instance.Status == fleet.StatusCreating || instance.Status == fleet.StatusCloning {
					m.creating[fleetName+"/"+instance.Name] = true
				}
			}
		}
	}

	return m
}

// ===========================================
// State Management
// ===========================================

// reload refreshes state and config from disk and prunes stale
// expanded instances. It does NOT rebuild rows — the active page
// is responsible for that.
func (m *model) reload() {
	// Persisted state + config come from the server now (GetState/GetConfig), not
	// from reading state.json/config.json directly — the TUI no longer touches
	// the files the server owns. The Watch stream keeps m.st fresh between
	// reloads (see the stateChangedMsg handler).
	st, err := fetchStateLegacy()
	if err != nil {
		m.err = err
		return
	}

	config, err := fetchConfigLegacy()
	if err != nil {
		m.err = err
		return
	}

	m.st = st
	m.config = config
	m.err = nil

	// (The control-socket listeners live on the server now — it owns every
	// running instance's socket and pushes browser.open as a Watch BrowserOpen
	// event; see watchBrowserOpenMsg.)

	// Auto-collapse expanded instances that are no longer running
	for _, ref := range m.sessionStore.ExpandedRefs() {
		if f, ok := st.Fleets[ref.Fleet]; ok {
			if instance, err := f.GetInstance(ref.Instance); err == nil && instance.Status == fleet.StatusRunning {
				continue
			}
		}
		m.sessionStore.CollapseAndForgetSessions(ref)
	}
}

// hydrateSavedGroups copies persisted pane layouts from state.json into
// the fleet page's in-memory map. Called once at startup so subsequent
// group restores use the layout geometry the user left behind rather
// than falling back to the default placeholder split.
func (m *model) hydrateSavedGroups() {
	if m.st == nil || m.fleetPage == nil {
		return
	}
	for stateKey, layout := range m.st.GroupLayouts {
		// Use the stateKey (instanceName/groupID) directly as the
		// savedGroups map key to maintain instance isolation.
		m.fleetPage.savedGroups[stateKey] = savedGroup{
			GroupID:      layout.GroupID,
			InstanceName: layout.InstanceName,
			Sessions:     layout.Sessions,
			Layout:       layout.Layout,
			PaneCount:    layout.PaneCount,
		}
	}
}

// pruneSavedGroupsForInstance drops saved layout entries for the given
// instance whose group IDs no longer appear in the latest session
// discovery. Called after each successful discovery so the next restart
// doesn't resurrect layouts for groups the user has already deleted.
func (m *model) pruneSavedGroupsForInstance(ref InstanceRef) {
	if m.st == nil || m.fleetPage == nil || !ref.Valid() {
		return
	}
	sanitized := SanitizeSessionName(ref.Instance)
	sessions := m.sessionStore.Sessions(ref)
	if len(sessions) == 0 {
		return
	}
	live := make(map[string]bool)
	for _, s := range sessions {
		if gid, ok := parseGroupID(sanitized, s.Name); ok {
			live[gid] = true
		}
	}
	if len(live) == 0 {
		return
	}
	for key, savedLayout := range m.fleetPage.savedGroups {
		if savedLayout.InstanceName != ref.Instance {
			continue
		}
		if !live[savedLayout.GroupID] {
			delete(m.fleetPage.savedGroups, key)
			delete(m.st.GroupLayouts, key)
			_ = deleteGroupLayoutRemote(savedLayout.InstanceName, savedLayout.GroupID)
		}
	}
}

// migrateRenamedSession rewrites the in-memory references that pointed at a
// just-renamed session so they track the new name. Renaming a grouped session
// changes the group ID, reprefixing every session in the group; without this
// migration the old group ID lingers in savedGroups (and gets re-persisted by
// the layout tick because activeGroup still names it), resurfacing as a
// duplicate row on the next TUI start and stranding the split on a session
// that no longer exists. Only called for a successful rename.
func (m *model) migrateRenamedSession(msg sessionRenamedMsg) {
	if m.fleetPage == nil || !msg.ref.Valid() {
		return
	}
	fleetPage := m.fleetPage
	sanitized := SanitizeSessionName(msg.ref.Instance)

	if msg.oldGroupID != "" {
		oldPrefix := sanitized + groupSep + msg.oldGroupID
		newPrefix := sanitized + groupSep + msg.newGroupID
		m.migrateSavedGroup(msg.ref, msg.oldGroupID, msg.newGroupID, oldPrefix, newPrefix)

		if fleetPage.activeGroup == (ActiveGroup{Ref: msg.ref, GroupID: msg.oldGroupID}) {
			fleetPage.activeGroup.GroupID = msg.newGroupID
			if fleetPage.splitPaneID != "" {
				unbindHostSplitKeys()
				bindHostSplitKeys(msg.ref.Key(), msg.newGroupID)
			}
		}
		if fleetPage.splitRef == msg.ref && strings.HasPrefix(fleetPage.splitSession, oldPrefix) {
			fleetPage.splitSession = newPrefix + fleetPage.splitSession[len(oldPrefix):]
		}
		if last, ok := m.sessionStore.LastActive(msg.ref); ok && last.groupID == msg.oldGroupID {
			last.groupID = msg.newGroupID
			if strings.HasPrefix(last.sessionName, oldPrefix) {
				last.sessionName = newPrefix + last.sessionName[len(oldPrefix):]
			}
			m.sessionStore.SetLastActive(msg.ref, last)
		}
		return
	}

	// Ungrouped rename: the pseudo group ID equals the session name, so the
	// active group and last-active entry track the name directly.
	if fleetPage.splitRef == msg.ref && fleetPage.splitSession == msg.oldName {
		fleetPage.splitSession = msg.newName
	}
	if fleetPage.activeGroup == (ActiveGroup{Ref: msg.ref, GroupID: msg.oldName}) {
		fleetPage.activeGroup.GroupID = msg.newName
	}
	if last, ok := m.sessionStore.LastActive(msg.ref); ok && last.sessionName == msg.oldName {
		last.sessionName = msg.newName
		if last.groupID == msg.oldName {
			last.groupID = msg.newName
		}
		m.sessionStore.SetLastActive(msg.ref, last)
	}
}

// migrateSavedGroup re-keys a persisted group layout from oldGroupID to
// newGroupID, rewriting the group ID and the session-name prefixes, and
// mirrors the move into state.json and the server. A no-op when no saved
// layout exists for the old group ID.
func (m *model) migrateSavedGroup(ref InstanceRef, oldGroupID, newGroupID, oldPrefix, newPrefix string) {
	oldKey := computeGroupKey(ref.Instance, oldGroupID)
	sg, ok := m.fleetPage.savedGroups[oldKey]
	if !ok {
		return
	}
	sessions := make([]string, len(sg.Sessions))
	for i, name := range sg.Sessions {
		if strings.HasPrefix(name, oldPrefix) {
			sessions[i] = newPrefix + name[len(oldPrefix):]
		} else {
			sessions[i] = name
		}
	}
	sg.GroupID = newGroupID
	sg.Sessions = sessions

	delete(m.fleetPage.savedGroups, oldKey)
	newKey := computeGroupKey(ref.Instance, newGroupID)
	m.fleetPage.savedGroups[newKey] = sg

	if m.st != nil && m.st.GroupLayouts != nil {
		delete(m.st.GroupLayouts, oldKey)
		m.st.GroupLayouts[newKey] = configutil.GroupLayout{
			GroupID:      sg.GroupID,
			InstanceName: sg.InstanceName,
			Sessions:     sg.Sessions,
			Layout:       sg.Layout,
			PaneCount:    sg.PaneCount,
		}
	}
	_ = deleteGroupLayoutRemote(ref.Instance, oldGroupID)
	_ = setGroupLayoutRemote(configutil.GroupLayout{
		GroupID:      sg.GroupID,
		InstanceName: sg.InstanceName,
		Sessions:     sg.Sessions,
		Layout:       sg.Layout,
		PaneCount:    sg.PaneCount,
	})
}

// pruneOrphanedSavedGroups drops saved layout entries whose instance no
// longer exists in state (e.g. the instance was deleted while fleet
// wasn't running). Called once at startup.
func (m *model) pruneOrphanedSavedGroups() {
	if m.st == nil || m.fleetPage == nil {
		return
	}
	live := make(map[string]bool)
	for _, f := range m.st.Fleets {
		for _, instance := range f.Instances {
			live[instance.Name] = true
		}
	}
	for key, savedLayout := range m.fleetPage.savedGroups {
		if !live[savedLayout.InstanceName] {
			delete(m.fleetPage.savedGroups, key)
			delete(m.st.GroupLayouts, key)
			_ = deleteGroupLayoutRemote(savedLayout.InstanceName, savedLayout.GroupID)
		}
	}
}

// ===========================================
// Helpers
// ===========================================

// firstFleetRepo returns the "owner/repo" string for the first fleet's
// remote URL, or "" if no fleets exist. Used to query GitHub APIs.
func (m *model) firstFleetRepo() string {
	if m.st == nil {
		return ""
	}
	for _, f := range m.st.Fleets {
		if f.Remote != "" {
			return repoFromRemote(f.Remote)
		}
	}
	return ""
}

// ===========================================
// Session Discovery
// ===========================================

// refreshInstanceSessions refreshes the given ref's session list from the
// server runtime (the server polls tmux for every running instance). It runs
// synchronously and returns nil — kept returning a tea.Cmd so the (now
// runtime-sourced) call sites after split-pane creation / group switching /
// session ops stay unchanged.
func (m *model) refreshInstanceSessions(ref InstanceRef) tea.Cmd {
	m.refreshSessionsFromRuntime(ref)
	return nil
}

// ===========================================
// Bubbletea Lifecycle
// ===========================================

// Init returns the initial set of commands for the program.
func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		m.spinner.Tick,
		m.agentSpinner.Tick,
		layoutTickCmd(),
		checkUpdateCmd(),
		updateCheckPollCmd(),
		checkReleaseNotesCmd(m.lastSeenVersion()),
		forceRepaintCmd(),
		// Live container status AND tmux session discovery are the server's job
		// now: it probes live status on the Watch subscribe edge + every 60s and
		// polls sessions ~1s, pushing both via Watch (StateChanged / RuntimeChanged).
		// The TUI renders sessions from the cached runtime — no client-side polling.
		// browser.open from in-container senders arrives as a Watch BrowserOpen
		// event (the server owns the control sockets); see watchBrowserOpenMsg.
		m.currentPage.Init(&m),
	}
	if len(m.creating) > 0 {
		cmds = append(cmds, pollCreatingCmd())
	}
	// Auto-fetch coder template parameters if template is configured
	if m.config != nil && m.config.CoderSettings.Template != "" {
		m.coderFetchingParams = true
		cmds = append(cmds, fetchCoderParamsCmd(m.config.CoderSettings.Template))
	}
	// Auto-fetch codespace machine types from the first fleet's repo
	if repo := m.firstFleetRepo(); repo != "" {
		m.codespaceFetchingMachines = true
		cmds = append(cmds, fetchCodespaceMachinesCmd(repo))
	}
	return tea.Batch(cmds...)
}

// Update handles a single Bubbletea message. Shared-only messages are
// handled here and returned early. Mixed messages handle their shared
// part then fall through. Everything else is forwarded to the active page.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// 0a. Resume after an execProcess child exited. bubbletea's
	// RestoreTerminal does not re-enable mouse tracking, so re-assert it
	// here, then dispatch the wrapped message through Update unchanged so
	// callers see the same behaviour as a bare tea.ExecProcess callback.
	// See execProcess / issue #88.
	if resume, ok := msg.(resumeMsg); ok {
		var next tea.Model = m
		var cmd tea.Cmd
		if resume.inner != nil {
			next, cmd = m.Update(resume.inner)
		}
		return next, tea.Batch(tea.EnableMouseCellMotion, cmd)
	}

	// 0. Mouse — left-button press moves the cursor to the clicked row
	// (on the fleet list and settings pages) and is then translated to
	// a key event so existing keyboard handlers do the rest: clicks on
	// instance rows fire Space (expand/collapse sessions), every other
	// click fires Enter. The wheel scrolls the settings viewport. Other
	// mouse events (motion, release, other buttons) are dropped: page
	// handlers only know about KeyMsgs.
	if mouseMsg, ok := msg.(tea.MouseMsg); ok {
		// Mouse wheel scrolls the settings viewport directly.
		if mouseMsg.Button == tea.MouseButtonWheelUp || mouseMsg.Button == tea.MouseButtonWheelDown {
			if page, ok := m.currentPage.(*settingsPage); ok && !page.editing {
				const wheelStep = 3
				if mouseMsg.Button == tea.MouseButtonWheelUp {
					page.scrollOffset -= wheelStep
				} else {
					page.scrollOffset += wheelStep
				}
				if page.scrollOffset < 0 {
					page.scrollOffset = 0
				}
			}
			return m, nil
		}
		if mouseMsg.Action == tea.MouseActionPress && mouseMsg.Button == tea.MouseButtonLeft {
			synthesizedKey := tea.KeyMsg{Type: tea.KeyEnter}
			hit := false
			switch page := m.currentPage.(type) {
			case *fleetPage:
				if page.mode == viewNormal && page.listRowY >= 0 {
					if idx := mouseMsg.Y - page.listRowY; idx >= 0 && idx < len(page.rows) && page.rows[idx].selectable() {
						page.cursor = idx
						hit = true
						if page.rows[idx].kind == rowInstance {
							synthesizedKey = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
						}
					}
				}
			case *settingsPage:
				if !page.editing && !page.showKeybindings {
					items := page.visibleItems(&m)
					for i, id := range items {
						y, ok := page.itemRowYs[id]
						if !ok {
							continue
						}
						rowHeight := page.itemHeights[id]
						if rowHeight <= 0 {
							rowHeight = 1
						}
						if mouseMsg.Y >= y && mouseMsg.Y < y+rowHeight {
							page.cursor = i
							hit = true
							break
						}
					}
				}
			}
			if !hit {
				return m, nil
			}
			msg = synthesizedKey
		} else {
			return m, nil
		}
	}

	// 1. Window size (shared)
	if windowSize, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = windowSize.Width
		m.height = windowSize.Height
	}

	// 2. Always update spinners
	var statusSpinCmd tea.Cmd
	m.spinner, statusSpinCmd = m.spinner.Update(msg)
	var agentSpinCmd tea.Cmd
	m.agentSpinner, agentSpinCmd = m.agentSpinner.Update(msg)
	spinCmd := tea.Batch(statusSpinCmd, agentSpinCmd)

	// 2.5 Release notes overlay — while shown it swallows all key input:
	// any key dismisses it (and persists the version) and is NOT
	// forwarded to the active page, so e.g. 'q' won't quit the app.
	if m.releaseNotesShowing() {
		if _, ok := msg.(tea.KeyMsg); ok {
			m.dismissReleaseNotes()
			return m, spinCmd
		}
	}

	// 3. Shared-only messages — return early
	switch msg := msg.(type) {
	case stateChangedMsg:
		// Read-path flip: the server's persisted snapshot drives the render
		// model. Convert to the legacy *configutil.State the views still read
		// from, and rebuild the row list. A StateChanged only fires on an actual
		// change (the hub diffs it), so this is not per-tick churn.
		m.pstate = msg.state
		if msg.state != nil {
			m.st = protoStateToLegacy(msg.state)
			if m.fleetPage != nil {
				m.fleetPage.buildRows(&m)
			}
		}
		return m, spinCmd

	case runtimeChangedMsg:
		// Cache the live runtime (merge by key), then refresh every expanded
		// instance's session list from it — the server polls tmux for all running
		// instances (~1s) and pushes it here, replacing the TUI's old client-side
		// session-discovery loop. Rebuild rows so the live data (agent activity,
		// stats, session list) renders.
		for _, r := range msg.runtime {
			m.runtime[rtKey(r.GetFleet(), r.GetInstance())] = r
		}
		for _, ref := range m.sessionStore.ExpandedRefs() {
			m.refreshSessionsFromRuntime(ref)
		}
		m.sessionStore.PruneStaleLastActive()
		if m.fleetPage != nil {
			m.fleetPage.buildRows(&m)
			// On the same ~1s cadence the old session-discovery tick ran on:
			// keep the split-pane layout snapshot honest against tmux-binding
			// mutations, and detect panes the user closed via an outer-tmux
			// binding (which bypasses fleet's handlers).
			fp := m.fleetPage
			if fp.splitPaneID != "" && !fp.activeGroup.Empty() && splitOpen() {
				fp.saveCurrentGroupLayout(m.st)
			}
			if fp.splitPaneID != "" && !splitOpen() {
				unbindHostSplitKeys()
				fp.clearSplit()
			}
		}
		return m, spinCmd

	case watchBrowserOpenMsg:
		// An in-container sender (e.g. a `fleet launch` TUI) asked the host to
		// open a URL; the server resolved it and pushed it over Watch. Open it in
		// the proxied local browser for the originating instance.
		if msg.url != "" && msg.fleet != "" && msg.instance != "" {
			return m, tea.Batch(spinCmd, m.openControlBrowserCmd(msg.fleet+"/"+msg.instance, msg.url))
		}
		return m, spinCmd

	case watchFileCopyMsg:
		// An in-container `fleet copy` (fc) asked the host to copy a file out of
		// the instance; pull it to this machine in the background (the requested
		// destination, or the downloads folder).
		if msg.fleet == "" || msg.instance == "" || msg.path == "" {
			return m, spinCmd
		}
		m.message = fmt.Sprintf("Copying %s from %s/%s...", msg.path, msg.fleet, msg.instance)
		return m, tea.Batch(spinCmd, copyInstanceFileCmd(msg.fleet, msg.instance, msg.path, msg.dest))

	case fileCopyDoneMsg:
		if msg.err != nil {
			m.message = fmt.Sprintf("Copy of %s failed: %v", msg.path, msg.err)
		} else {
			m.message = fmt.Sprintf("Copied %s -> %s", msg.path, msg.dest)
		}
		return m, spinCmd

	case remoteMcpStatusMsg:
		// The server pushed the current remote-MCP tunnel status (connection
		// state + computed Public MCP URL). Cache it for the settings page to
		// render; a redraw picks it up on the next frame.
		m.remoteMcpStatus = msg.status
		return m, spinCmd

	case watchErrMsg, watchClosedMsg:
		// The watcher reconnects on its own; nothing rendered from the stream
		// yet in Step 5, so this is a no-op (do not crash on a dropped stream).
		return m, spinCmd

	case updateCheckMsg:
		if msg.latestVersion != "" {
			m.updateAvailable = msg.latestVersion
		}
		return m, spinCmd

	case updateCheckTickMsg:
		// Re-check for updates and re-arm the periodic tick so users who
		// leave fleet open get notified about releases published mid-session.
		return m, tea.Batch(spinCmd, checkUpdateCmd(), updateCheckPollCmd())

	case releaseNotesMsg:
		// Only surface the overlay when there's an actual body to show;
		// an empty message means dev build, already seen, or fetch error.
		if msg.version != "" && strings.TrimSpace(msg.body) != "" {
			m.releaseNotesVersion = msg.version
			m.releaseNotesBody = msg.body
		}
		return m, spinCmd

	case updateInstalledMsg:
		if msg.err != nil {
			m.message = fmt.Sprintf("Update failed: %v", msg.err)
			return m, spinCmd
		}
		// Installer succeeded. Record the new binary, quit the TUI,
		// and let Run() syscall.Exec into it — replacing the current
		// process so the new fleet is NOT nested inside the old one.
		m.pendingExecPath = msg.path
		m.pendingExecArgs = msg.args
		m.quitting = true
		return m, tea.Quit

	case coderParamsFetchedMsg:
		m.coderFetchingParams = false
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to fetch parameters: %v", msg.err)
			return m, spinCmd
		}
		if m.config == nil {
			m.config = configutil.DefaultConfig()
		}
		// Merge parameters: keep existing user-set values, add new ones with defaults
		existing := make(map[string]string)
		for _, param := range m.config.CoderSettings.Parameters {
			if param.Value != "" {
				existing[param.Name] = param.Value
			}
		}
		var newParams []configutil.CoderParameter
		for _, fetchedParam := range msg.params {
			existingValue := existing[fetchedParam.Name]
			newParams = append(newParams, configutil.CoderParameter{
				Name:         fetchedParam.Name,
				Value:        existingValue,
				DefaultValue: fetchedParam.DefaultValue,
				DisplayName:  fetchedParam.DisplayName,
				Description:  fetchedParam.Description,
				Type:         fetchedParam.Type,
			})
		}
		m.config.CoderSettings.Parameters = newParams
		m.coderPresets = nil
		m.coderPresets = append(m.coderPresets, msg.presets...)
		if m.config.CoderSettings.Preset == "" && len(m.coderPresets) > 0 {
			m.config.CoderSettings.Preset = m.coderPresets[0]
		}
		_ = setConfigRemote(m.config)
		m.message = fmt.Sprintf("Loaded %d parameters, %d presets", len(newParams), len(m.coderPresets))
		return m, spinCmd

	case codespaceMachinesFetchedMsg:
		m.codespaceFetchingMachines = false
		if msg.err != nil {
			return m, spinCmd
		}
		m.codespaceMachines = msg.machines
		if m.config != nil && m.config.CodespacesSettings.Machine == "" && len(m.codespaceMachines) > 0 {
			m.config.CodespacesSettings.Machine = m.codespaceMachines[0].Name
			_ = setConfigRemote(m.config)
		}
		return m, spinCmd

	case forceRepaintTickMsg:
		// Scrub artifacts left by outer-tmux pane resizes without
		// flicker, AND self-heal a stale window size. We emit a
		// synthetic WindowSizeMsg carrying the *actual* current pty
		// dimensions (read via ioctl, exactly what bubbletea's own
		// SIGWINCH handler reads) rather than the cached m.width/
		// m.height.
		//
		// Why the live read matters: opening/closing sessions and
		// restoring layouts fires a burst of pane resizes, and SIGWINCH
		// does not queue — the burst coalesces, so bubbletea can read
		// the pty at an intermediate moment and never get a fresh
		// signal for the final settled geometry. That left m.width
		// stale (too wide), so the TUI rendered lines wider than the
		// pane and tmux wrapped them — garbled output that only a
		// manual divider drag (a fresh SIGWINCH) corrected. Reading the
		// true size here recovers within one tick with no manual drag.
		// When the size is unchanged this is a pure repaint.
		//
		// The repaint itself: the WindowSizeMsg invalidates bubbletea's
		// per-line cache; on the next flush every tracked line is
		// rewritten with EraseLineRight appended, scrubbing stale chars
		// inside the TUI's bounds. The trailing EraseScreenBelow escape
		// that View() tacks onto the last line then clears everything
		// beneath the TUI — and because that escape is part of the view
		// string, the clear lands in the same atomic buffer flush as
		// the redraw, so the terminal never sees a blank frame (unlike
		// tea.ClearScreen which writes the erase ahead of the next
		// render tick).
		w, h := currentTermSize()
		if w == 0 || h == 0 {
			w, h = m.width, m.height
		}
		return m, tea.Batch(
			spinCmd,
			func() tea.Msg { return tea.WindowSizeMsg{Width: w, Height: h} },
			forceRepaintCmd(),
		)
	}

	// 4. Mixed messages — handle shared part, then forward to page
	var extraCmds []tea.Cmd
	switch msg := msg.(type) {
	case layoutTickMsg:
		// Fast outer-tmux layout poll. Snapshots the active group's
		// layout into savedGroups every 250ms so Ctrl+Q / Ctrl+O — which
		// kill panes via an outer tmux binding that bypasses fleet —
		// can't race ahead of the save. The diff gate makes idle ticks
		// free. Always reschedule so the tick keeps firing.
		fleetPage := m.fleetPage
		if fleetPage != nil && fleetPage.splitPaneID != "" && !fleetPage.activeGroup.Empty() && splitOpen() {
			fleetPage.saveCurrentGroupLayout(m.st)
		}
		extraCmds = append(extraCmds, layoutTickCmd())

	case operationDoneMsg:
		m.reload()
		if msg.err != nil {
			m.message = fmt.Sprintf("Error: %v", msg.err)
		} else {
			m.message = msg.message
		}

	case instanceCreateErrMsg:
		// The dispatch (or the server's pre-create) failed, so there is no
		// half-created record for the client to annotate — the server owns
		// failure status for any record it did create. Just clear the optimistic
		// "creating" marker, refresh, and surface the error.
		key := msg.fleet + "/" + msg.instance
		delete(m.creating, key)
		m.reload()
		m.message = fmt.Sprintf("Failed to create %s: %v", key, msg.err)

	case splitPaneMsg:
		// err and paneID are independent: a restoreGroupCmd run can
		// partially succeed (some splits OK, some failed after retries),
		// in which case both are set — show the error AND wire up the
		// panes that did make it.
		fleetPage := m.fleetPage
		if !fleetPage.finishGroupRestore(msg.restoreSeq) {
			break
		}
		if msg.err != nil {
			m.message = fmt.Sprintf("failed to do tmux split pane: %v", msg.err)
		}
		if msg.paneID != "" {
			// splitOpenedAt/splitViaRestore are retained for the layout-snapshot
			// bookkeeping in clearSplit (the per-session event log moved to the
			// server, which owns ~/.fleet/fleet.log).
			fleetPage.splitOpenedAt = time.Now()
			fleetPage.splitViaRestore = msg.restoreSeq != 0
			fleetPage.splitPaneID = msg.paneID
			fleetPage.splitRef = msg.ref
			fleetPage.splitSession = msg.session
			fleetPage.activeGroup = ActiveGroup{Ref: msg.ref, GroupID: msg.groupID}
			m.sessionStore.SetLastActive(msg.ref, lastSession{sessionName: msg.session, groupID: msg.groupID})
			bindHostSplitKeys(msg.ref.Key(), splitBindGroupID(msg.ref.Instance, msg.session, msg.groupID))
			extraCmds = append(extraCmds, m.refreshInstanceSessions(msg.ref))
		}

	case sessionCreatedMsg:
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to create session: %v", msg.err)
		} else {
			m.message = "Session created"
		}
		// The new session appears on the next ~1s runtime tick; nudge from the
		// current runtime cache so the list reflects what's already known.
		m.refreshSessionsFromRuntime(msg.ref)

	case sessionRenamedMsg:
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to rename session: %v", msg.err)
		} else {
			m.message = fmt.Sprintf("Renamed session %s → %s", msg.oldName, msg.newName)
			m.migrateRenamedSession(msg)
		}
		// Refresh the discovery cache but skip pruning: the runtime cache
		// may still contain old session names until the next poll, so
		// pruneSavedGroupsForInstance would incorrectly treat the newly
		// re-keyed group as not-live and delete it. The next periodic
		// runtime update will prune any truly dead groups.
		if msg.ref.Valid() && m.sessionStore.IsExpanded(msg.ref) {
			m.sessionStore.SetDiscovery(msg.ref, m.runtimeSessions(msg.ref))
		}

	case sessionDeletedMsg:
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to delete session: %v", msg.err)
		} else {
			m.message = fmt.Sprintf("Deleted session %s", msg.sessionName)
		}
		if last, ok := m.sessionStore.LastActive(msg.ref); ok {
			if last.sessionName == msg.sessionName || (msg.groupID != "" && last.groupID == msg.groupID) {
				m.sessionStore.ClearLastActive(msg.ref)
			}
		}
		fleetPage := m.fleetPage
		if msg.groupID != "" {
			key := computeGroupKey(msg.ref.Instance, msg.groupID)
			delete(fleetPage.savedGroups, key)
			if m.st != nil && m.st.GroupLayouts != nil {
				delete(m.st.GroupLayouts, key)
				_ = deleteGroupLayoutRemote(msg.ref.Instance, msg.groupID)
			}
		}
		// Tear down the split only when the deletion targets the very
		// group/session we're attached to — must match instance too,
		// since group IDs are not unique across instances.
		deletedActive := fleetPage.splitRef == msg.ref &&
			(fleetPage.splitSession == msg.sessionName ||
				(msg.groupID != "" && fleetPage.activeGroup.GroupID == msg.groupID))
		if deletedActive {
			if fleetPage.splitPaneID != "" {
				killAllSplitPanes()
				unbindHostSplitKeys()
			}
			fleetPage.clearSplit()
		}
		m.refreshSessionsFromRuntime(msg.ref)

	case pollCreatingTickMsg:
		if len(m.creating) > 0 {
			m.reload()
			for key := range m.creating {
				parts := strings.SplitN(key, "/", 2)
				if len(parts) != 2 {
					continue
				}
				fleetName, instName := parts[0], parts[1]
				if f, ok := m.st.Fleets[fleetName]; ok {
					if instance, err := f.GetInstance(instName); err == nil {
						switch instance.Status {
						case fleet.StatusRunning:
							delete(m.creating, key)
							warnPath := fleetpaths.WarnPath(fleetName, instName)
							if warnData, err := os.ReadFile(warnPath); err == nil {
								_ = os.Remove(warnPath)
								firstLine := strings.SplitN(strings.TrimSpace(string(warnData)), "\n", 2)[0]
								m.message = fmt.Sprintf("Instance %s is running — %s (press l for details)", key, firstLine)
							} else {
								m.message = fmt.Sprintf("Instance %s is running (container: %s)",
									key, instance.ContainerID[:min(12, len(instance.ContainerID))])
							}
						case fleet.StatusFailed:
							delete(m.creating, key)
							fleetPage := m.fleetPage
							if instance.Backend == fleet.BackendCodespaces && strings.HasPrefix(instance.Error, codespaceerr.AuthScope) {
								fleetPage.mode = viewCodespacesAuth
								fleetPage.dialogFleet = fleetName
								fleetPage.dialogInst = instName
								m.message = ""
							} else if instance.Backend == fleet.BackendCodespaces && strings.HasPrefix(instance.Error, codespaceerr.Machine) {
								fleetPage.mode = viewCodespacesMachine
								fleetPage.dialogFleet = fleetName
								fleetPage.dialogInst = instName
								m.message = ""
							} else if instance.Backend == fleet.BackendCodespaces && strings.HasPrefix(instance.Error, codespaceerr.Limit) {
								fleetPage.mode = viewCodespacesLimit
								fleetPage.dialogFleet = fleetName
								fleetPage.dialogInst = instName
								m.message = ""
							} else {
								m.message = fmt.Sprintf("Failed to create %s: %s", key, instance.Error)
							}
						}
					}
				}
			}
			if len(m.creating) > 0 {
				extraCmds = append(extraCmds, pollCreatingCmd())
			}
		}

	case instanceSpawnedMsg:
		// The server pre-created the record; pull it into view (the TUI still
		// renders from m.st) and start polling it to running.
		m.reload()
		if m.fleetPage != nil {
			m.fleetPage.buildRows(&m)
		}
		extraCmds = append(extraCmds, pollCreatingCmd())

	case groupCycleMsg:
		fleetPage := m.fleetPage
		if msg.seq == fleetPage.debounceSeq && !fleetPage.pendingGroup.Empty() {
			cmd := fleetPage.commitGroupCycle(&m)
			extraCmds = append(extraCmds, cmd)
		}
	}

	// 5. Forward to current page
	pageCmd := m.currentPage.Update(&m, msg)

	// 6. Return
	allCmds := []tea.Cmd{spinCmd, pageCmd}
	allCmds = append(allCmds, extraCmds...)
	return m, tea.Batch(allCmds...)
}

// View renders the current page. On quit it cleans up split panes
// and port forwards.
func (m model) View() string {
	if m.quitting {
		// Clean up split panes via the fleet page. Snapshot the current
		// tmux layout BEFORE killing panes so pane-size changes made
		// since the last group switch are persisted and replayed on the
		// next fleet startup.
		//
		// Bubbletea can call View() more than once while tearing down
		// after tea.Quit. Clear splitPaneID after the first pass so a
		// subsequent call doesn't re-enter this block — without that,
		// the second saveCurrentGroupLayout reads the post-kill tmux
		// layout (1 pane, TUI only) and overwrites the correct save
		// with a truncated single-pane record.
		fleetPage := m.fleetPage
		if fleetPage.splitPaneID != "" {
			fleetPage.saveCurrentGroupLayout(m.st)
			killAllSplitPanes()
			fleetPage.splitPaneID = ""
			fleetPage.restoringGroupID = ""
		}
		m.portForwards.Shutdown()
		if m.inHostTmux {
			unbindHostSessionCycleKeys()
			unbindHostSplitKeys()
			unbindHostCloseKeys()
			unbindRefocusTUIKey()
		}
		return ""
	}
	// Append EraseScreenBelow (\x1b[0J) so that whenever bubbletea
	// rewrites the last line, it also scrubs any stale characters
	// sitting beneath the TUI. This is a no-op while the last line's
	// content is unchanged (bubbletea's line-diff skips the line, the
	// escape is not re-executed). On the 1-second repaint tick the
	// accompanying WindowSizeMsg invalidates the full line cache,
	// forcing the last line — and thus this escape — to be rewritten
	// inside the same atomic buffer flush as the rest of the redraw.
	// Release notes overlay takes over the whole screen, centered.
	if m.releaseNotesShowing() {
		return m.viewReleaseNotes() + "\x1b[0J"
	}
	return m.currentPage.View(&m) + "\x1b[0J"
}

// ===========================================
// Sorting Helper
// ===========================================

// sortedFleetNames returns fleet names in stable alphabetical order.
func sortedFleetNames(fleets map[string]*fleet.Fleet) []string {
	names := make([]string, 0, len(fleets))
	for name := range fleets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ===========================================
// Entry Point
// ===========================================

// Run starts the TUI.
func Run() error {
	// Install (or refresh) the Fleet Admiral skill into ~/.claude/skills so the
	// user's coding agent can orchestrate fleet. Best-effort and hash-gated:
	// it's a cheap no-op once installed. The error is deliberately ignored — a
	// skill-install hiccup must never block startup, and client code does not
	// write the server-owned event log.
	_ = admiralskill.EnsureInstalled()

	// Register the fleet MCP server in the user's Claude Code config
	// (~/.claude.json) so an agent can actually CALL the tools that skill
	// teaches. Backgrounded because on a cold start the endpoint files only
	// appear once the auto-spawned daemon binds its MCP listener; the helper
	// polls for them and gives up silently — same best-effort contract as the
	// skill install, an MCP-install hiccup must never block (or slow) startup.
	go admiralmcp.EnsureInstalledEventually(30 * time.Second)

	m := newModel()

	// Start clipboard buffer polling when running inside tmux.
	// A goroutine polls `tmux show-buffer` and copies changes to the
	// system clipboard (wl-copy / xclip / pbcopy). This is the
	// universal clipboard mechanism that works on ALL terminals,
	// including those without OSC 52 support.
	var clipCancel context.CancelFunc
	if m.inHostTmux {
		if clipSync := newClipboardSync(); clipSync != nil {
			ctx, cancel := context.WithCancel(context.Background())
			clipCancel = cancel
			go clipSync.Start(ctx)
		}
	}

	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())

	// Subscribe to the fleet server's Watch stream for the TUI's lifetime,
	// injecting events into the program. In P2 Step 5 these only populate the
	// m.pstate / m.runtime caches; the View still renders from the legacy fields
	// until the read-path flip (Steps 6–7).
	watchCtx, watchCancel := context.WithCancel(context.Background())
	go runWatchStream(watchCtx, program)

	finalModel, err := program.Run()
	watchCancel()

	if clipCancel != nil {
		clipCancel()
	}

	// (The control-socket listeners live on the server now; nothing host-side to
	// tear down. The TUI session-lifecycle log moved to the server's fleet.log.)

	// Release the mutation connection (the Watch connection is closed by its own
	// goroutine when watchCtx is cancelled above).
	closeMutationConn()

	// If the user just performed a successful auto-update, replace
	// the current process with the freshly installed binary. Doing
	// the exec here (in the old fleet's Go process, after the TUI
	// has fully torn down) means the new fleet takes over this
	// process ID — it is NOT a child of the old fleet. That way ^C
	// in the new fleet exits cleanly instead of dropping back into
	// the old fleet.
	if err == nil {
		if finalAppModel, ok := finalModel.(model); ok && finalAppModel.pendingExecPath != "" {
			if execErr := syscall.Exec(finalAppModel.pendingExecPath, finalAppModel.pendingExecArgs, os.Environ()); execErr != nil {
				return fmt.Errorf("failed to launch updated fleet %q: %w", finalAppModel.pendingExecPath, execErr)
			}
			// syscall.Exec does not return on success.
		}
	}
	return err
}
