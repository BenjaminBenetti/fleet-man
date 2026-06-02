package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	codespacesbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/codespaces"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/portforward"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ===========================================
// Model
// ===========================================

type model struct {
	st     *state.State
	config *state.Config
	err    error

	// Page routing
	currentPage Page
	fleetPage   *fleetPage // persistent — has running state accessed by background message handlers

	spinner      spinner.Model
	agentSpinner spinner.Model   // pulse throbber rendered next to running agents' instance names
	creating     map[string]bool // "fleet/instance" keys currently being created

	backends map[fleet.BackendType]backend.Backend // one per backend type, lazily created
	stats    map[string]*backend.ContainerStats    // containerID → stats
	activity *ActivityTracker                      // agent working/waiting/idle detection

	// reprovisioning dedupes in-flight Claude hook reinstall attempts
	// per containerID. The capture loop fires every few seconds, and a
	// reinstall on a slow backend can outlast one tick — without
	// dedup we'd stack concurrent provisions for the same container.
	// LoadOrStore inserts a sentinel; the goroutine clears it on exit.
	// Pointer-typed because the bubbletea model is passed by value,
	// which would otherwise copy the sync.Map's internal lock.
	reprovisioning *sync.Map // containerID → struct{}

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

	// control owns one control-socket listener per running instance and
	// funnels received envelopes (e.g. an in-instance `fleet launch` TUI's
	// "browser.open" requests) into a channel the Update loop drains. Its
	// listener set is kept in step with the running instances by reload().
	control *controlRegistry

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
		creating:       make(map[string]bool),
		backends:       make(map[fleet.BackendType]backend.Backend),
		stats:          make(map[string]*backend.ContainerStats),
		activity:       NewActivityTracker(),
		reprovisioning: &sync.Map{},
		portForwards:   portforward.NewManager(),
		activeBrowser:  make(map[string]string),
		control:        newControlRegistry(),
		sessionStore:   NewSessionStore(),
		spinner:        spinnerModel,
		agentSpinner:   agentSpinnerModel,
		inHostTmux:     os.Getenv("TMUX") != "",
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
	// if anything is missing. "First startup" = the ~/.fleet/ dir doesn't exist.
	if _, err := os.Stat(state.FleetDir()); os.IsNotExist(err) {
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
	st, err := state.Load()
	if err != nil {
		m.err = err
		return
	}

	config, err := state.LoadConfig()
	if err != nil {
		m.err = err
		return
	}

	m.st = st
	m.config = config
	m.err = nil

	// Keep the control-socket listeners in step with the running set: start
	// listeners for newly-running instances, drop them for stopped/gone ones.
	// Idempotent, so funnelling every state refresh through here is cheap.
	if m.control != nil {
		m.control.syncRunning(st)
	}

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
	changed := false
	for key, savedLayout := range m.fleetPage.savedGroups {
		if savedLayout.InstanceName != ref.Instance {
			continue
		}
		if !live[savedLayout.GroupID] {
			delete(m.fleetPage.savedGroups, key)
			delete(m.st.GroupLayouts, key)
			changed = true
		}
	}
	if changed {
		_ = state.Save(m.st)
	}
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
	changed := false
	for key, savedLayout := range m.fleetPage.savedGroups {
		if !live[savedLayout.InstanceName] {
			delete(m.fleetPage.savedGroups, key)
			delete(m.st.GroupLayouts, key)
			changed = true
		}
	}
	if changed {
		_ = state.Save(m.st)
	}
}

// ===========================================
// Backend Helpers
// ===========================================

// backendFor returns the cached backend for the given type, creating it lazily.
func (m *model) backendFor(backendType fleet.BackendType) backend.Backend {
	if backendType == "" {
		backendType = fleet.BackendDevcontainer
	}
	if instanceBackend, ok := m.backends[backendType]; ok {
		return instanceBackend
	}
	instanceBackend := backendutil.New(backendType, false)
	m.backends[backendType] = instanceBackend
	return instanceBackend
}

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

// instanceBackend returns the backend for the given instance's backend type.
// For codespaces, it registers the real codespace name so that exec calls
// use the correct name instead of deriving from the workspace path.
func (m *model) instanceBackend(instance *fleet.Instance) backend.Backend {
	backendImpl := m.backendFor(instance.Backend)
	if instance.Backend == fleet.BackendCodespaces && instance.ContainerID != "" {
		if csb, ok := backendImpl.(*codespacesbackend.CodespacesBackend); ok {
			csb.RegisterName(instance.WorkspaceDir, instance.ContainerID)
		}
	}
	return backendImpl
}

// containersByBackend groups running instances by their backend type.
func (m *model) containersByBackend() map[fleet.BackendType]*backendGroup {
	groups := make(map[fleet.BackendType]*backendGroup)
	for _, f := range m.st.Fleets {
		for _, instance := range f.Instances {
			if instance.ContainerID == "" || instance.Status != fleet.StatusRunning {
				continue
			}
			backendType := instance.Backend
			if backendType == "" {
				backendType = fleet.BackendDevcontainer
			}
			g, ok := groups[backendType]
			if !ok {
				g = &backendGroup{}
				groups[backendType] = g
			}
			g.ids = append(g.ids, instance.ContainerID)
		}
	}
	return groups
}

// reinstallMissingClaudeHooks scans the latest captures for the
// "Claude hook script missing" signal and re-runs the provisioner
// for any container that reports it. Fire-and-forget: each reinstall
// runs in its own goroutine so the TUI message loop is never
// blocked, and a sync.Map dedupes concurrent attempts per container
// in case provisioning takes longer than the capture interval.
//
// Failures are non-fatal — the next capture tick will trigger
// another attempt — but each failure is surfaced as a per-instance
// warning so the user can spot a persistently-failing reinstall.
func (m *model) reinstallMissingClaudeHooks(screens map[string]backend.AllSessions) {
	if m.st == nil {
		return
	}
	for cid, capture := range screens {
		if !capture.OK || !capture.ClaudeHookMissing {
			continue
		}
		fleetName, instance := m.findInstanceByContainerID(cid)
		if instance == nil {
			continue
		}
		if _, busy := m.reprovisioning.LoadOrStore(cid, struct{}{}); busy {
			continue
		}
		backendImpl := m.instanceBackend(instance)
		wsDir := instance.WorkspaceDir
		instanceName := instance.Name
		go func(id string) {
			defer m.reprovisioning.Delete(id)
			executor := agentdetect.NewBackendExecutor(backendImpl, wsDir)
			if err := agentdetect.NewClaudeProvisioner(executor).Provision(); err != nil {
				state.WriteWarn(fleetName, instanceName, fmt.Sprintf("claude hook reinstall failed: %v", err))
			}
		}(cid)
	}
}

// findInstanceByContainerID returns the fleet name and instance for
// the given containerID, or "", nil when no running instance matches.
func (m *model) findInstanceByContainerID(containerID string) (string, *fleet.Instance) {
	if m.st == nil {
		return "", nil
	}
	for fleetName, f := range m.st.Fleets {
		for _, instance := range f.Instances {
			if instance.ContainerID == containerID {
				return fleetName, instance
			}
		}
	}
	return "", nil
}

// ===========================================
// Session Discovery
// ===========================================

// sessionDiscoveryLoop returns a tea.Cmd that lists tmux sessions for
// expanded instances on a 1-second cycle.
func (m model) sessionDiscoveryLoop() tea.Cmd {
	return sessionDiscoveryCmd(m.backends, m.sessionStore.ExpandedRefs(), m.st.Fleets)
}

// refreshInstanceSessions returns a tea.Cmd that re-lists tmux sessions
// for the given ref (if expanded). Used after split pane creation,
// group switching, and session creation to keep the UI in sync.
func (m *model) refreshInstanceSessions(ref InstanceRef) tea.Cmd {
	if !ref.Valid() || !m.sessionStore.IsExpanded(ref) {
		return nil
	}
	f, ok := m.st.Fleets[ref.Fleet]
	if !ok {
		return nil
	}
	instance, err := f.GetInstance(ref.Instance)
	if err != nil {
		return nil
	}
	return listSessionsCmd(m.instanceBackend(instance), instance.WorkspaceDir, ref)
}

// ===========================================
// Stats
// ===========================================

// fetchAllStatsCmd creates a command that fetches stats from all backends concurrently.
func (m model) fetchAllStatsCmd(delay bool) tea.Cmd {
	groups := m.containersByBackend()
	if len(groups) == 0 {
		return fetchStatsCmd(nil, nil, delay)
	}

	type fetchInput struct {
		instanceBackend backend.Backend
		ids             []string
	}
	var inputs []fetchInput
	for backendType, g := range groups {
		inputs = append(inputs, fetchInput{
			instanceBackend: m.backendFor(backendType),
			ids:             g.ids,
		})
	}

	// If only one backend type, use the simple path
	if len(inputs) == 1 {
		return fetchStatsCmd(inputs[0].instanceBackend, inputs[0].ids, delay)
	}

	// Multiple backend types: fetch concurrently and merge
	return func() tea.Msg {
		if delay {
			time.Sleep(3 * time.Second)
		}

		allStats := make(map[string]*backend.ContainerStats)
		allScreens := make(map[string]backend.AllSessions)
		allProbes := make(map[string]string)
		var allIDs []string

		type result struct {
			stats   map[string]*backend.ContainerStats
			screens map[string]backend.AllSessions
			probes  map[string]string
			ids     []string
		}

		ch := make(chan result, len(inputs))
		for _, input := range inputs {
			go func(instanceBackend backend.Backend, ids []string) {
				stats, _ := instanceBackend.Stats(ids)
				screens := backend.CaptureAllSessionsForAll(instanceBackend, ids)
				probes := backend.AgentToolProbes(instanceBackend, ids)
				ch <- result{stats, screens, probes, ids}
			}(input.instanceBackend, input.ids)
		}

		for range inputs {
			r := <-ch
			for k, v := range r.stats {
				allStats[k] = v
			}
			for k, v := range r.screens {
				allScreens[k] = v
			}
			for k, v := range r.probes {
				allProbes[k] = v
			}
			allIDs = append(allIDs, r.ids...)
		}

		return statsMsg{stats: allStats, screens: allScreens, probes: allProbes, containerIDs: allIDs}
	}
}

// ===========================================
// Bubbletea Lifecycle
// ===========================================

// Init returns the initial set of commands for the program.
func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		m.spinner.Tick,
		m.agentSpinner.Tick,
		m.fetchAllStatsCmd(false),
		m.sessionDiscoveryLoop(),
		layoutTickCmd(),
		checkUpdateCmd(),
		updateCheckPollCmd(),
		checkReleaseNotesCmd(m.lastSeenVersion()),
		forceRepaintCmd(),
		// Probe live state right away so a fleet started after a long
		// idle (e.g. overnight) reflects containers that stopped while
		// fleet was offline before the user even sees the list. The
		// periodic tick that follows handles drift during the session.
		refreshLiveStatusCmd(collectLiveStatusProbes(m.st)),
		liveStatusPollCmd(),
		// Drain control-socket events from in-instance senders (the listeners
		// themselves were started by the reload() inside newModel()). The
		// model-level Update re-arms this waiter after each event.
		waitForControlEventCmd(m.control.events),
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
					if idx := mouseMsg.Y - page.listRowY; idx >= 0 && idx < len(page.rows) {
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
	case statsMsg:
		if msg.stats != nil {
			m.stats = msg.stats
		}
		if msg.screens != nil {
			m.activity.Update(msg.screens, msg.probes, msg.containerIDs, time.Now())
			m.reinstallMissingClaudeHooks(msg.screens)
		}
		return m, tea.Batch(spinCmd, m.fetchAllStatsCmd(true))

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
			m.config = state.DefaultConfig()
		}
		// Merge parameters: keep existing user-set values, add new ones with defaults
		existing := make(map[string]string)
		for _, param := range m.config.CoderSettings.Parameters {
			if param.Value != "" {
				existing[param.Name] = param.Value
			}
		}
		var newParams []state.CoderParameter
		for _, fetchedParam := range msg.params {
			existingValue := existing[fetchedParam.Name]
			newParams = append(newParams, state.CoderParameter{
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
		for _, preset := range msg.presets {
			m.coderPresets = append(m.coderPresets, preset.Name)
		}
		if m.config.CoderSettings.Preset == "" && len(m.coderPresets) > 0 {
			m.config.CoderSettings.Preset = m.coderPresets[0]
		}
		_ = state.SaveConfig(m.config)
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
			_ = state.SaveConfig(m.config)
		}
		return m, spinCmd

	case liveStatusTickMsg:
		// Rearm the periodic refresh and kick off a probe pass over
		// the current snapshot of instances. Probe goroutines run
		// independently and feed back via liveStatusMsg.
		return m, tea.Batch(
			spinCmd,
			refreshLiveStatusCmd(collectLiveStatusProbes(m.st)),
			liveStatusPollCmd(),
		)

	case liveStatusMsg:
		// Reconcile persisted state with what each backend reports.
		// applyLiveStatuses writes through to disk when anything
		// changed; reload() then refreshes the in-memory view so the
		// fleet page renders the corrected statuses on the next draw.
		if applyLiveStatuses(m.st, msg.updates) {
			m.reload()
		}
		return m, spinCmd

	case controlEventMsg:
		// An in-instance sender (e.g. a `fleet launch` TUI) wrote an Envelope
		// to that instance's control socket. Dispatch by type, then re-arm the
		// waiter so the single in-flight command keeps draining the channel.
		// msg is already the concrete controlEventMsg here (the switch binds
		// it), so convert it to the underlying controlEvent directly.
		event := controlEvent(msg)
		var cmd tea.Cmd
		switch event.env.Type {
		case control.TypeOpenBrowser:
			var payload control.OpenBrowserPayload
			if json.Unmarshal(event.env.Payload, &payload) == nil && payload.URL != "" {
				cmd = m.openControlBrowserCmd(event.instanceKey, payload.URL)
			}
		}
		return m, tea.Batch(cmd, waitForControlEventCmd(m.control.events))

	case forceRepaintTickMsg:
		// Scrub artifacts left by outer-tmux pane resizes without
		// flicker. A synthetic WindowSizeMsg (with the current
		// dimensions so no app-level resize happens) causes
		// bubbletea's renderer to invalidate its per-line cache; on
		// the next flush every tracked line is rewritten with
		// EraseLineRight appended, scrubbing stale chars inside the
		// TUI's bounds. The trailing EraseScreenBelow escape that
		// View() tacks onto the last line then clears everything
		// beneath the TUI — and because that escape is part of the
		// view string, the clear lands in the same atomic buffer
		// flush as the redraw, so the terminal never sees a blank
		// frame (unlike tea.ClearScreen which writes the erase
		// ahead of the next render tick).
		return m, tea.Batch(
			spinCmd,
			func() tea.Msg { return tea.WindowSizeMsg{Width: m.width, Height: m.height} },
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

	case sessionDiscoveryMsg:
		if msg.discovered != nil {
			for ref, sessions := range msg.discovered {
				m.sessionStore.SetDiscovery(ref, sessions)
			}
			m.sessionStore.PruneStaleLastActive()
			for ref := range msg.discovered {
				m.pruneSavedGroupsForInstance(ref)
			}
		}
		extraCmds = append(extraCmds, m.sessionDiscoveryLoop())

	case operationDoneMsg:
		m.reload()
		if msg.err != nil {
			m.message = fmt.Sprintf("Error: %v", msg.err)
		} else {
			m.message = msg.message
		}

	case instanceCreateErrMsg:
		key := msg.fleet + "/" + msg.instance
		delete(m.creating, key)
		st, _ := state.Load()
		if st != nil {
			if f, ok := st.Fleets[msg.fleet]; ok {
				if instance, err := f.GetInstance(msg.instance); err == nil {
					instance.Status = fleet.StatusFailed
					instance.Error = msg.err.Error()
					_ = state.Save(st)
				}
			}
		}
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
			// Log "session opened" only for direct (splitPaneCmd) opens, which
			// run the command in the pane with no `fleet shell` subprocess.
			// Group restores (restoreSeq != 0) spawn `fleet shell` per pane,
			// and that process logs its own open/close — logging here too
			// would duplicate it. splitOpenedAt/splitViaRestore pair with the
			// "session closed" log in clearSplit.
			fleetPage.splitOpenedAt = time.Now()
			fleetPage.splitViaRestore = msg.restoreSeq != 0
			if !fleetPage.splitViaRestore {
				logSessionOpen("split", msg.ref.Fleet, msg.ref.Instance, msg.session, msg.command)
			}
			fleetPage.splitPaneID = msg.paneID
			fleetPage.splitRef = msg.ref
			fleetPage.splitSession = msg.session
			fleetPage.activeGroup = ActiveGroup{Ref: msg.ref, GroupID: msg.groupID}
			m.sessionStore.SetLastActive(msg.ref, lastSession{sessionName: msg.session, groupID: msg.groupID})
			bindHostSplitKeys(msg.ref.Key(), msg.groupID)
			extraCmds = append(extraCmds, m.refreshInstanceSessions(msg.ref))
		}

	case sessionsMsg:
		if msg.err != nil {
			m.sessionStore.SetDiscoveryError(msg.ref, msg.err)
		} else {
			m.sessionStore.SetDiscovery(msg.ref, msg.sessions)
			m.pruneSavedGroupsForInstance(msg.ref)
		}

	case sessionCreatedMsg:
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to create session: %v", msg.err)
		} else {
			m.message = "Session created"
		}
		if m.sessionStore.IsExpanded(msg.ref) {
			if f, ok := m.st.Fleets[msg.ref.Fleet]; ok {
				if instance, err := f.GetInstance(msg.ref.Instance); err == nil {
					extraCmds = append(extraCmds, listSessionsCmd(m.instanceBackend(instance), instance.WorkspaceDir, msg.ref))
				}
			}
		}

	case sessionRenamedMsg:
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to rename session: %v", msg.err)
		} else {
			m.message = fmt.Sprintf("Renamed session %s → %s", msg.oldName, msg.newName)
		}
		if m.sessionStore.IsExpanded(msg.ref) {
			if f, ok := m.st.Fleets[msg.ref.Fleet]; ok {
				if instance, err := f.GetInstance(msg.ref.Instance); err == nil {
					extraCmds = append(extraCmds, listSessionsCmd(m.instanceBackend(instance), instance.WorkspaceDir, msg.ref))
				}
			}
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
				_ = state.Save(m.st)
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
		if m.sessionStore.IsExpanded(msg.ref) {
			if f, ok := m.st.Fleets[msg.ref.Fleet]; ok {
				if instance, err := f.GetInstance(msg.ref.Instance); err == nil {
					extraCmds = append(extraCmds, listSessionsCmd(m.instanceBackend(instance), instance.WorkspaceDir, msg.ref))
				}
			}
		}

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
							warnPath := state.WarnPath(fleetName, instName)
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
							if instance.Backend == fleet.BackendCodespaces && strings.HasPrefix(instance.Error, codespacesbackend.ErrPrefixAuthScope) {
								fleetPage.mode = viewCodespacesAuth
								fleetPage.dialogFleet = fleetName
								fleetPage.dialogInst = instName
								m.message = ""
							} else if instance.Backend == fleet.BackendCodespaces && strings.HasPrefix(instance.Error, codespacesbackend.ErrPrefixMachine) {
								fleetPage.mode = viewCodespacesMachine
								fleetPage.dialogFleet = fleetName
								fleetPage.dialogInst = instName
								m.message = ""
							} else if instance.Backend == fleet.BackendCodespaces && strings.HasPrefix(instance.Error, codespacesbackend.ErrPrefixLimit) {
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
	start := time.Now()
	flog.Info("fleet TUI started")
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
	finalModel, err := program.Run()
	flog.Info("fleet TUI stopped", "ms", flog.MillisSince(start))

	if clipCancel != nil {
		clipCancel()
	}

	// Tear down every control-socket listener so the host releases the socket
	// files it created. The registry pointer in m is shared with the program's
	// copy of the model, so this Closes the same servers the loop was draining.
	m.control.shutdown()

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
