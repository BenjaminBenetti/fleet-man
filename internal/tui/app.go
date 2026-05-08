package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	codespacesbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/codespaces"
	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
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

	spinner  spinner.Model
	creating map[string]bool // "fleet/instance" keys currently being created

	backends map[fleet.BackendType]backend.Backend // one per backend type, lazily created
	stats    map[string]*backend.ContainerStats    // containerID → stats
	activity *ActivityTracker                      // agent working/waiting/idle detection

	coderPresets        []string // available preset names (in-memory, from API)
	coderFetchingParams bool     // true while fetching template parameters

	codespaceMachines         []codespaceMachine // available machine types (from GitHub API)
	codespaceFetchingMachines bool               // true while fetching machine types

	toolStatus []deps.ToolStatus // cached tool install statuses for settings page

	// Port forwarding
	portForwards *portforward.Manager // manages active port forward processes

	// Instance expansion: which instances are expanded to show sessions
	expandedInstances map[string]bool              // key: "fleet/instance"
	sessions          map[string]*sessionDiscovery // key: "fleet/instance"

	// Last active session per instance (in-memory only). Used to
	// reconnect to the previous session when pressing enter on an
	// instance row instead of always creating a new one.
	lastActive map[string]lastSession // key: "fleet/instance"

	// Split pane mode: when fleet runs inside a host tmux session,
	// pressing enter opens the instance shell in a right-side pane
	// instead of suspending the TUI.
	inHostTmux bool // true when TMUX env var is set at startup

	// Update check
	updateAvailable string // non-empty = new version tag from GitHub

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

	m := model{
		creating:          make(map[string]bool),
		backends:          make(map[fleet.BackendType]backend.Backend),
		stats:             make(map[string]*backend.ContainerStats),
		activity:          NewActivityTracker(),
		portForwards:      portforward.NewManager(),
		expandedInstances: make(map[string]bool),
		sessions:          make(map[string]*sessionDiscovery),
		lastActive:        make(map[string]lastSession),
		spinner:           spinnerModel,
		inHostTmux:        os.Getenv("TMUX") != "",
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
				if instance.Status == fleet.StatusCreating {
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

	// Auto-collapse expanded instances that are no longer running
	for key := range m.expandedInstances {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) == 2 {
			if f, ok := st.Fleets[parts[0]]; ok {
				if instance, err := f.GetInstance(parts[1]); err == nil && instance.Status == fleet.StatusRunning {
					continue
				}
			}
		}
		delete(m.expandedInstances, key)
		delete(m.sessions, key)
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
	for gid, layout := range m.st.GroupLayouts {
		m.fleetPage.savedGroups[gid] = savedGroup{
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
func (m *model) pruneSavedGroupsForInstance(instKey string) {
	if m.st == nil || m.fleetPage == nil {
		return
	}
	parts := strings.SplitN(instKey, "/", 2)
	if len(parts) != 2 {
		return
	}
	instanceName := parts[1]
	sanitized := SanitizeSessionName(instanceName)
	disc, ok := m.sessions[instKey]
	if !ok || disc == nil || disc.err != nil {
		return
	}
	live := make(map[string]bool)
	for _, s := range disc.sessions {
		if gid, ok := parseGroupID(sanitized, s.Name); ok {
			live[gid] = true
		}
	}
	changed := false
	for gid, savedLayout := range m.fleetPage.savedGroups {
		if savedLayout.InstanceName != instanceName {
			continue
		}
		if !live[gid] {
			delete(m.fleetPage.savedGroups, gid)
			delete(m.st.GroupLayouts, gid)
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
	for gid, savedLayout := range m.fleetPage.savedGroups {
		if !live[savedLayout.InstanceName] {
			delete(m.fleetPage.savedGroups, gid)
			delete(m.st.GroupLayouts, gid)
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

// backendGroup holds container IDs grouped by backend type. Sessions
// no longer need to be tracked here — CaptureAllSessions discovers
// every tmux session inside each container at capture time.
type backendGroup struct {
	ids []string
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

// ===========================================
// Session Discovery
// ===========================================

// sessionDiscoveryLoop returns a tea.Cmd that lists tmux sessions for
// expanded instances on a 1-second cycle.
func (m model) sessionDiscoveryLoop() tea.Cmd {
	return sessionDiscoveryCmd(m.backends, m.expandedInstances, m.st.Fleets)
}

// refreshInstanceSessions returns a tea.Cmd that re-lists tmux sessions
// for the given instance (if expanded). Used after split pane creation,
// group switching, and session creation to keep the UI in sync.
func (m *model) refreshInstanceSessions(instanceName string) tea.Cmd {
	for instKey, expanded := range m.expandedInstances {
		if !expanded {
			continue
		}
		parts := strings.SplitN(instKey, "/", 2)
		if len(parts) != 2 || parts[1] != instanceName {
			continue
		}
		if f, ok := m.st.Fleets[parts[0]]; ok {
			if instance, err := f.GetInstance(parts[1]); err == nil {
				instanceBackend := m.instanceBackend(instance)
				return listSessionsCmd(instanceBackend, instance.WorkspaceDir, instKey)
			}
		}
	}
	return nil
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
		m.fetchAllStatsCmd(false),
		m.sessionDiscoveryLoop(),
		layoutTickCmd(),
		checkUpdateCmd(),
		forceRepaintCmd(),
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
	// click fires Enter. Other mouse events (motion, wheel, release,
	// other buttons) are dropped: page handlers only know about KeyMsgs.
	if mouseMsg, ok := msg.(tea.MouseMsg); ok {
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
						h := page.itemHeights[id]
						if h <= 0 {
							h = 1
						}
						if mouseMsg.Y >= y && mouseMsg.Y < y+h {
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

	// 2. Always update spinner
	var spinCmd tea.Cmd
	m.spinner, spinCmd = m.spinner.Update(msg)

	// 3. Shared-only messages — return early
	switch msg := msg.(type) {
	case statsMsg:
		if msg.stats != nil {
			m.stats = msg.stats
		}
		if msg.screens != nil {
			m.activity.Update(msg.screens, msg.probes, msg.containerIDs, time.Now())
		}
		return m, tea.Batch(spinCmd, m.fetchAllStatsCmd(true))

	case updateCheckMsg:
		if msg.latestVersion != "" {
			m.updateAvailable = msg.latestVersion
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
		for _, p := range m.config.CoderSettings.Parameters {
			if p.Value != "" {
				existing[p.Name] = p.Value
			}
		}
		var newParams []state.CoderParameter
		for _, rp := range msg.params {
			existingValue := existing[rp.Name]
			newParams = append(newParams, state.CoderParameter{
				Name:         rp.Name,
				Value:        existingValue,
				DefaultValue: rp.DefaultValue,
				DisplayName:  rp.DisplayName,
				Description:  rp.Description,
				Type:         rp.Type,
			})
		}
		m.config.CoderSettings.Parameters = newParams
		m.coderPresets = nil
		for _, p := range msg.presets {
			m.coderPresets = append(m.coderPresets, p.Name)
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
		fp := m.fleetPage
		if fp != nil && fp.splitPaneID != "" && fp.activeGroupID != "" && splitOpen() {
			fp.saveCurrentGroupLayout(m.st)
		}
		extraCmds = append(extraCmds, layoutTickCmd())

	case sessionDiscoveryMsg:
		if msg.discovered != nil {
			for key, sessions := range msg.discovered {
				m.sessions[key] = &sessionDiscovery{sessions: sessions, fetchedAt: time.Now()}
			}
			// Clear lastActive entries that point to sessions/groups
			// that no longer exist.
			for key, last := range m.lastActive {
				disc, ok := m.sessions[key]
				if !ok || disc.err != nil {
					delete(m.lastActive, key)
					continue
				}
				if !sessionStillExists(last, disc.sessions) {
					delete(m.lastActive, key)
				}
			}
			for key := range msg.discovered {
				m.pruneSavedGroupsForInstance(key)
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
		if msg.err != nil {
			m.message = fmt.Sprintf("failed to do tmux split pane: %v", msg.err)
		}
		if msg.paneID != "" {
			fp := m.fleetPage
			fp.splitPaneID = msg.paneID
			fp.splitFleet = msg.fleet
			fp.splitInstance = msg.instance
			fp.splitSession = msg.session
			fp.activeGroupID = msg.groupID
			instKey := msg.fleet + "/" + msg.instance
			m.lastActive[instKey] = lastSession{sessionName: msg.session, groupID: msg.groupID}
			bindHostSplitKeys(instKey, msg.groupID)
			extraCmds = append(extraCmds, m.refreshInstanceSessions(msg.instance))
		}

	case sessionsMsg:
		if msg.err != nil {
			m.sessions[msg.instanceKey] = &sessionDiscovery{err: msg.err, fetchedAt: time.Now()}
		} else {
			m.sessions[msg.instanceKey] = &sessionDiscovery{sessions: msg.sessions, fetchedAt: time.Now()}
			m.pruneSavedGroupsForInstance(msg.instanceKey)
		}

	case sessionCreatedMsg:
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to create session: %v", msg.err)
		} else {
			m.message = "Session created"
		}
		if m.expandedInstances[msg.instanceKey] {
			parts := strings.SplitN(msg.instanceKey, "/", 2)
			if len(parts) == 2 {
				if f, ok := m.st.Fleets[parts[0]]; ok {
					if instance, err := f.GetInstance(parts[1]); err == nil {
						instanceBackend := m.instanceBackend(instance)
						extraCmds = append(extraCmds, listSessionsCmd(instanceBackend, instance.WorkspaceDir, msg.instanceKey))
					}
				}
			}
		}

	case sessionRenamedMsg:
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to rename session: %v", msg.err)
		} else {
			m.message = fmt.Sprintf("Renamed session %s → %s", msg.oldName, msg.newName)
		}
		if m.expandedInstances[msg.instanceKey] {
			parts := strings.SplitN(msg.instanceKey, "/", 2)
			if len(parts) == 2 {
				if f, ok := m.st.Fleets[parts[0]]; ok {
					if instance, err := f.GetInstance(parts[1]); err == nil {
						instanceBackend := m.instanceBackend(instance)
						extraCmds = append(extraCmds, listSessionsCmd(instanceBackend, instance.WorkspaceDir, msg.instanceKey))
					}
				}
			}
		}

	case sessionDeletedMsg:
		if msg.err != nil {
			m.message = fmt.Sprintf("Failed to delete session: %v", msg.err)
		} else {
			m.message = fmt.Sprintf("Deleted session %s", msg.sessionName)
		}
		if last, ok := m.lastActive[msg.instanceKey]; ok {
			if last.sessionName == msg.sessionName || last.groupID == msg.groupID {
				delete(m.lastActive, msg.instanceKey)
			}
		}
		fp := m.fleetPage
		if fp.splitSession == msg.sessionName || (msg.groupID != "" && fp.activeGroupID == msg.groupID) {
			if fp.splitPaneID != "" {
				killAllSplitPanes()
				unbindHostSplitKeys()
			}
			fp.splitPaneID = ""
			fp.splitFleet = ""
			fp.splitInstance = ""
			fp.splitSession = ""
			fp.activeGroupID = ""
		}
		if m.expandedInstances[msg.instanceKey] {
			parts := strings.SplitN(msg.instanceKey, "/", 2)
			if len(parts) == 2 {
				if f, ok := m.st.Fleets[parts[0]]; ok {
					if instance, err := f.GetInstance(parts[1]); err == nil {
						instanceBackend := m.instanceBackend(instance)
						extraCmds = append(extraCmds, listSessionsCmd(instanceBackend, instance.WorkspaceDir, msg.instanceKey))
					}
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
							warnPath := filepath.Join(state.FleetDir(), "logs", fleetName+"-"+instName+".warn")
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
							fp := m.fleetPage
							if instance.Backend == fleet.BackendCodespaces && strings.HasPrefix(instance.Error, codespacesbackend.ErrPrefixAuthScope) {
								fp.mode = viewCodespacesAuth
								fp.dialogFleet = fleetName
								fp.dialogInst = instName
								m.message = ""
							} else if instance.Backend == fleet.BackendCodespaces && strings.HasPrefix(instance.Error, codespacesbackend.ErrPrefixMachine) {
								fp.mode = viewCodespacesMachine
								fp.dialogFleet = fleetName
								fp.dialogInst = instName
								m.message = ""
							} else if instance.Backend == fleet.BackendCodespaces && strings.HasPrefix(instance.Error, codespacesbackend.ErrPrefixLimit) {
								fp.mode = viewCodespacesLimit
								fp.dialogFleet = fleetName
								fp.dialogInst = instName
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
		fp := m.fleetPage
		if msg.seq == fp.debounceSeq && fp.pendingGroupID != "" {
			cmd := fp.commitGroupCycle(&m)
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
		fp := m.fleetPage
		if fp.splitPaneID != "" {
			fp.saveCurrentGroupLayout(m.st)
			killAllSplitPanes()
			fp.splitPaneID = ""
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
	m := newModel()

	// Start clipboard buffer polling when running inside tmux.
	// A goroutine polls `tmux show-buffer` and copies changes to the
	// system clipboard (wl-copy / xclip / pbcopy). This is the
	// universal clipboard mechanism that works on ALL terminals,
	// including those without OSC 52 support.
	var clipCancel context.CancelFunc
	if m.inHostTmux {
		if cs := newClipboardSync(); cs != nil {
			ctx, cancel := context.WithCancel(context.Background())
			clipCancel = cancel
			go cs.Start(ctx)
		}
	}

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	finalModel, err := p.Run()

	if clipCancel != nil {
		clipCancel()
	}

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
