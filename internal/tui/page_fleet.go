package tui

import (
	"encoding/hex"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/devcontainersetup"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/gitutil"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// ===========================================
// Fleet Page
// ===========================================

var resolveWorkspaceBranch = gitutil.BranchName

// fleetPage holds fleet-list-specific state.
type fleetPage struct {
	rows      []row
	cursor    int
	collapsed map[string]bool

	// focusedFleet, when non-empty, names the single fleet shown in focus
	// mode. All other fleets are hidden from the list, the help bar is hidden,
	// the "settings" row becomes "[ leave focus ]", and q/esc leave focus
	// instead of quitting (focus behaves like a dialog).
	focusedFleet string

	mode                    viewMode
	dialogFleet             string
	dialogInst              string
	dialogBackend           fleet.BackendType
	dialogColor             string
	dialogRow               int
	dialogEditing           bool
	dialogFieldActive       bool
	dialogGroupID           string
	dialogSession           string
	dialogClaudeMount       bool
	dialogCodexMount        bool
	dialogGhMount           bool
	dialogAuggieMount       bool
	dialogBuildkitServer    bool
	dialogDebCache          bool
	dialogImageCache        bool
	dialogPreferFleetLaunch bool
	// dialogPreferFleetLaunchSet tracks whether PreferFleetLaunch should be
	// persisted as an explicit value. It starts true only if the fleet already
	// had a value, and flips true when the user toggles that row — so the
	// instant-save path never collapses a "never asked" (nil) PreferFleetLaunch
	// into an explicit false just because the user edited an unrelated setting.
	dialogPreferFleetLaunchSet bool

	// Caching section (edit-fleet dialog) state. The buildkit, deb, and image
	// cache rows share one interaction model (toggle + [Delete cache] button +
	// inline confirm + in-flight spinner); the per-row sub-state below applies to
	// whichever cache row currently has the dialog cursor.
	dialogCachingExpanded    bool      // ▼ Caching expanded, revealing the cache rows
	dialogCacheButtonFocused bool      // horizontal sub-cursor: on the [Delete cache] button vs the toggle
	dialogDeleteCacheConfirm bool      // inline confirm armed (first Enter on the button)
	dialogDeleting           bool      // a cache-wipe RPC is in flight
	dialogDeletingKind       cacheKind // which cache the in-flight wipe targets (valid only while dialogDeleting)

	// Custom mounts section (edit-fleet dialog) state.
	dialogCustomMountsExpanded bool     // ▼ Custom mounts expanded, revealing per-mount rows + the add row
	dialogCustomMounts         []string // working copy of the fleet's custom mounts (instant-save)
	dialogAddingMount          bool     // true while the "+ Add mount" text input is active
	dialogCustomMountErr       string   // inline validation error shown under the add-mount input
	dialogMountRemoveConfirm   bool     // inline "[remove?]" confirm armed on the focused custom-mount row (mirrors the Caching [Delete cache] flow)

	// Layouts section (edit-fleet dialog) state. The preset creation/edit flow
	// itself lives in lpFlow while mode == viewLayoutPreset.
	dialogLayoutsExpanded     bool                 // ▼ Layouts expanded, revealing per-preset rows + the add row
	dialogLayoutPresets       []fleet.LayoutPreset // working copy of the fleet's layout presets (instant-save)
	dialogPresetRemoveFocused bool                 // horizontal sub-cursor: on the [remove] button vs the preset row (mirrors dialogCacheButtonFocused)
	dialogPresetRemoveConfirm bool                 // inline "[remove?]" confirm armed on the focused preset row
	lpFlow                    *layoutPresetFlow    // open preset creation/edit flow (nil unless mode == viewLayoutPreset)

	// New-session dialog template cycling: the fleet's presets snapshotted at
	// dialog open, and the Tab-cycled selection (-1 = none, a plain session).
	dialogPresets   []fleet.LayoutPreset
	dialogPresetIdx int

	dialogDetecting bool // true while a homedir auto-detect cmd is in flight

	// dialogBrowserSwitching is true while the switch-browser dialog
	// has dispatched a kill+relaunch but the browserProxyMsg has not
	// yet returned. Used to swap the dialog body for a spinner so the
	// user isn't staring at a static prompt during the SIGTERM grace
	// period.
	dialogBrowserSwitching bool

	// dialogPendingRepoURL is the repo URL the user submitted in the
	// new-fleet dialog. It is held here across the asynchronous
	// devcontainer inspection so the inspect-result handler can fall
	// through into either adding the fleet or showing the
	// no-devcontainer prompt without re-asking the user.
	dialogPendingRepoURL   string
	dialogPendingFleetName string

	textInput        textinput.Model
	branchInput      textinput.Model
	homedirInput     textinput.Model
	customMountInput textinput.Model

	pfCursor int

	splitPaneID     string
	splitRef        InstanceRef // (fleet, instance) of the open split pane; zero when none
	splitSession    string
	splitOpenedAt   time.Time // when the current split pane was opened; for "session closed" duration
	splitViaRestore bool      // true when the split was opened via restoreGroupCmd (fleet shell logs its own open/close, so the TUI must not duplicate it)

	activeGroup      ActiveGroup // qualified by splitRef so two groups with the same ID across instances cannot alias
	savedGroups      map[string]savedGroup
	pendingGroup     ActiveGroup
	debounceSeq      int
	restoringGroupID string
	restoreSeq       int

	// listRowY is the terminal Y (0-indexed) where rows[0] is rendered,
	// recorded during View() so mouse clicks can be mapped back to a
	// row index. -1 means "not yet rendered" or "no clickable rows".
	listRowY int

	// Armada selector: a target embedded in the list box's TOP BORDER line
	// ("╭─ Armada [ local ] ──╮"). It is part of the j/k navigation cycle as a
	// virtual stop ABOVE the first row: armadaFocused is true while the cursor
	// is on it (up from the top row focuses it; up again wraps to the bottom).
	// Enter/Space (or the `A` key, or a mouse click) opens the dropdown.
	// armadaDialogRow is the dropdown cursor while mode == viewArmadaSelect.
	// armadaY + armadaX0/X1 record the label's on-screen position and column
	// span during View() for mouse hit-testing (-1 = not rendered).
	armadaFocused   bool
	armadaDialogRow int
	armadaY         int
	armadaX0        int
	armadaX1        int
}

// newFleetPage creates a new fleet page with default state.
func newFleetPage() *fleetPage {
	nameInput := textinput.New()
	nameInput.Placeholder = "instance-name"
	nameInput.CharLimit = 64

	branchInput := textinput.New()
	branchInput.Placeholder = "default branch"
	branchInput.CharLimit = 128

	homedirInput := textinput.New()
	homedirInput.Placeholder = "/home/vscode"
	homedirInput.CharLimit = 256

	customMountInput := textinput.New()
	customMountInput.Placeholder = "/opt/data"
	customMountInput.CharLimit = 256

	return &fleetPage{
		collapsed:        make(map[string]bool),
		savedGroups:      make(map[string]savedGroup),
		textInput:        nameInput,
		branchInput:      branchInput,
		homedirInput:     homedirInput,
		customMountInput: customMountInput,
		listRowY:         -1,
		armadaY:          -1,
	}
}

func (fleetPage *fleetPage) restoreInProgress() bool {
	return fleetPage.restoringGroupID != ""
}

func (fleetPage *fleetPage) beginGroupRestore(groupID string) int {
	fleetPage.restoreSeq++
	fleetPage.restoringGroupID = groupID
	return fleetPage.restoreSeq
}

func (fleetPage *fleetPage) finishGroupRestore(seq int) bool {
	if seq == 0 {
		return true
	}
	if seq != fleetPage.restoreSeq {
		return false
	}
	fleetPage.restoringGroupID = ""
	return true
}

// clearSplit resets every field that tracks the open split pane. Used
// whenever the split is closed (user toggle, external kill, restore
// teardown) so a future open starts from a known-empty state.
func (fleetPage *fleetPage) clearSplit() {
	// (The per-session open/close event log moved to the server, which owns
	// ~/.fleet/fleet.log; the split bookkeeping below is host-side only.)
	fleetPage.splitPaneID = ""
	fleetPage.splitRef = InstanceRef{}
	fleetPage.splitSession = ""
	fleetPage.splitOpenedAt = time.Time{}
	fleetPage.splitViaRestore = false
	fleetPage.activeGroup = ActiveGroup{}
	fleetPage.restoringGroupID = ""
}

// Init is called when the fleet page becomes active.
func (fleetPage *fleetPage) Init(m *model) tea.Cmd {
	fleetPage.buildRows(m)
	return nil
}

// Update dispatches messages to the appropriate handler based on the
// current view mode.
func (fleetPage *fleetPage) Update(m *model, msg tea.Msg) tea.Cmd {
	// Fleet-specific async messages that need row rebuilds. (Session-list
	// refresh + the split-layout snapshot / external-kill detection ride the
	// runtimeChangedMsg handler in the model-level Update now — the server pushes
	// session changes on the runtime stream, replacing the old client poll.)
	switch msg.(type) {
	case operationDoneMsg:
		fleetPage.buildRows(m)
		return nil

	case instanceCreateErrMsg:
		fleetPage.buildRows(m)
		return nil

	case browserProxyMsg:
		proxyMsg := msg.(browserProxyMsg)
		if proxyMsg.err != nil {
			m.message = fmt.Sprintf("Browser proxy error: %v", proxyMsg.err)
		} else {
			m.message = fmt.Sprintf("Browser opened (proxy on localhost:%d)", proxyMsg.localPort)
			// Record which instance the browser bound to this data dir now
			// serves, so a later control-socket open for a different instance
			// switches the browser over rather than new-tabbing into the wrong
			// proxy. Recompute the data dir the same way the open path did.
			if fleetName, instanceName, ok := splitInstanceKey(proxyMsg.instanceKey); ok {
				dataDir := browserDataDir(fleetName, instanceName, multipleBrowsersPerFleet(m))
				if m.activeBrowser == nil {
					m.activeBrowser = make(map[string]string)
				}
				m.activeBrowser[dataDir] = proxyMsg.instanceKey
			}
		}
		// Tear down the switching-spinner dialog if it was active.
		if fleetPage.dialogBrowserSwitching {
			fleetPage.dialogBrowserSwitching = false
			fleetPage.mode = viewNormal
		}
		return nil

	case homedirDetectedMsg:
		return fleetPage.handleHomedirDetected(m, msg.(homedirDetectedMsg))

	case deleteCacheDoneMsg:
		return fleetPage.handleDeleteCacheDone(m, msg.(deleteCacheDoneMsg))

	case devcontainerInspectedMsg:
		return fleetPage.handleDevcontainerInspected(m, msg.(devcontainerInspectedMsg))
	}

	// Mode-specific dispatch
	switch fleetPage.mode {
	case viewConfirmDelete:
		return fleetPage.updateConfirmDelete(m, msg)
	case viewConfirmRebuild:
		return fleetPage.updateConfirmRebuild(m, msg)
	case viewConfirmDeleteFleetWarn:
		return fleetPage.updateConfirmDeleteFleetWarn(m, msg)
	case viewAddInstance:
		return fleetPage.updateAddInstance(m, msg)
	case viewAddFleet:
		return fleetPage.updateAddFleet(m, msg)
	case viewAddFleetInspecting:
		return fleetPage.updateAddFleetInspecting(m, msg)
	case viewAddFleetNoDevcontainer:
		return fleetPage.updateAddFleetNoDevcontainer(m, msg)
	case viewEditFleet:
		return fleetPage.updateEditFleet(m, msg)
	case viewTagInstance:
		return fleetPage.updateTagInstance(m, msg)
	case viewPortForward:
		return fleetPage.updatePortForward(m, msg)
	case viewCodespacesAuth:
		return fleetPage.updateCodespacesAuth(m, msg)
	case viewCodespacesLimit:
		return fleetPage.updateCodespacesLimit(m, msg)
	case viewCodespacesMachine:
		return fleetPage.updateCodespacesMachine(m, msg)
	case viewConfirmDeleteSession:
		return fleetPage.updateConfirmDeleteSession(m, msg)
	case viewConfirmBrowserSwitch:
		return fleetPage.updateConfirmBrowserSwitch(m, msg)
	case viewChooseBrowserLaunch:
		return fleetPage.updateChooseBrowserLaunch(m, msg)
	case viewArmadaSelect:
		return fleetPage.updateArmadaSelect(m, msg)
	case viewCreateSession:
		return fleetPage.updateCreateSession(m, msg)
	case viewLayoutPreset:
		return fleetPage.updateLayoutPreset(m, msg)
	case viewRenameSession:
		return fleetPage.updateRenameSession(m, msg)
	case viewCloneInstance:
		return fleetPage.updateCloneInstance(m, msg)
	default:
		return fleetPage.updateNormal(m, msg)
	}
}

// View renders the fleet list page.
func (fleetPage *fleetPage) View(m *model) string {
	return fleetPage.viewFleetList(m)
}

// ===========================================
// Row Management
// ===========================================

// buildRows rebuilds the navigable row list from the current state.
func (fleetPage *fleetPage) buildRows(m *model) {
	// Remember whether the cursor sat on the trailing action row (settings or
	// its focus-mode replacement) so it stays pinned there across the rebuild.
	wasOnActionRow := false
	if r := fleetPage.currentRow(); r != nil && (r.kind == rowSettings || r.kind == rowLeaveFocus) {
		wasOnActionRow = true
	}

	fleetPage.rows = nil

	// A focused fleet that no longer exists (deleted while focused) drops focus
	// so the list doesn't render empty.
	if fleetPage.focusedFleet != "" {
		if _, ok := m.st.Fleets[fleetPage.focusedFleet]; !ok {
			fleetPage.focusedFleet = ""
		}
	}

	names := sortedFleetNames(m.st.Fleets)

	for _, name := range names {
		// Focus mode renders only the focused fleet; everything else is hidden.
		if fleetPage.focusedFleet != "" && name != fleetPage.focusedFleet {
			continue
		}
		f := m.st.Fleets[name]
		fleetPage.rows = append(fleetPage.rows, row{kind: rowFleetHeader, fleetName: name})
		if !fleetPage.collapsed[name] {
			for _, instance := range f.Instances {
				fleetPage.rows = append(fleetPage.rows, row{kind: rowInstance, fleetName: name, instance: instance})
				ref := InstanceRef{Fleet: name, Instance: instance.Name}
				if m.sessionStore.IsExpanded(ref) {
					if instance.Tag != "" {
						fleetPage.rows = append(fleetPage.rows, row{kind: rowInstanceTag, fleetName: name, instance: instance})
					}
					liveGroups := make(map[string]bool)
					for _, g := range m.sessionStore.Groups(ref) {
						liveGroups[g.GroupID] = true
						rootName := g.Sessions[0].Name
						fleetPage.rows = append(fleetPage.rows, row{
							kind:        rowSession,
							fleetName:   name,
							instance:    instance,
							sessionName: rootName,
							groupID:     g.GroupID,
							groupSize:   len(g.Sessions),
						})
					}
					fleetPage.appendSavedGroupRows(name, instance, liveGroups)
					fleetPage.rows = append(fleetPage.rows, row{
						kind:      rowNewSession,
						fleetName: name,
						instance:  instance,
					})
				}
			}
		}
	}
	if fleetPage.focusedFleet != "" {
		fleetPage.rows = append(fleetPage.rows, row{kind: rowLeaveFocus})
	} else {
		fleetPage.rows = append(fleetPage.rows, row{kind: rowSettings})
	}
	if wasOnActionRow {
		fleetPage.cursor = len(fleetPage.rows) - 1
	}
	if fleetPage.cursor >= len(fleetPage.rows) {
		fleetPage.cursor = max(0, len(fleetPage.rows)-1)
	}
	// A rebuild can shift rows so the cursor lands on a display-only row
	// (e.g. a tag line inserted above a session row); nudge it forward.
	if r := fleetPage.currentRow(); r != nil && !r.selectable() {
		fleetPage.moveCursor(1)
	}
}

// enterFocus switches the list into focus mode for the named fleet, hiding all
// other fleets and parking the cursor on the focused fleet's header.
func (fleetPage *fleetPage) enterFocus(m *model, name string) {
	if name == "" {
		return
	}
	if _, ok := m.st.Fleets[name]; !ok {
		return
	}
	fleetPage.focusedFleet = name
	fleetPage.armadaFocused = false
	fleetPage.buildRows(m)
	fleetPage.cursorToFleetHeader(name)
	m.message = ""
}

// leaveFocus exits focus mode and restores the cursor to the fleet that was
// focused so the user keeps their place in the full list. It also drops any
// Armada-selector focus so the row cursor is visible again afterwards.
func (fleetPage *fleetPage) leaveFocus(m *model) {
	if fleetPage.focusedFleet == "" {
		return
	}
	name := fleetPage.focusedFleet
	fleetPage.focusedFleet = ""
	fleetPage.armadaFocused = false
	fleetPage.buildRows(m)
	fleetPage.cursorToFleetHeader(name)
	m.message = ""
}

// cursorToFleetHeader points the cursor at the named fleet's header row, if it
// is present in the current row list.
func (fleetPage *fleetPage) cursorToFleetHeader(name string) {
	for i, r := range fleetPage.rows {
		if r.kind == rowFleetHeader && r.fleetName == name {
			fleetPage.cursor = i
			return
		}
	}
}

func (fleetPage *fleetPage) appendSavedGroupRows(fleetName string, instance *fleet.Instance, liveGroups map[string]bool) {
	sanitized := SanitizeSessionName(instance.Name)
	for _, group := range fleetPage.savedGroupsForInstance(instance.Name) {
		if liveGroups[group.GroupID] {
			continue
		}
		sessions := savedGroupSessionNames(group, sanitized)
		fleetPage.rows = append(fleetPage.rows, row{
			kind:        rowSession,
			fleetName:   fleetName,
			instance:    instance,
			sessionName: sessions[0],
			groupID:     group.GroupID,
			groupSize:   savedGroupPaneCount(group),
		})
	}
}

func (fleetPage *fleetPage) savedGroupsForInstance(instanceName string) []savedGroup {
	groups := make([]savedGroup, 0, len(fleetPage.savedGroups))
	for _, group := range fleetPage.savedGroups {
		if group.InstanceName == instanceName {
			groups = append(groups, group)
		}
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].GroupID < groups[j].GroupID
	})
	return groups
}

// currentRow returns a pointer to the row at the cursor position.
func (fleetPage *fleetPage) currentRow() *row {
	if fleetPage.cursor < 0 || fleetPage.cursor >= len(fleetPage.rows) {
		return nil
	}
	return &fleetPage.rows[fleetPage.cursor]
}

// firstSelectable returns the index of the first selectable row (-1 if none).
func (fleetPage *fleetPage) firstSelectable() int {
	for i, r := range fleetPage.rows {
		if r.selectable() {
			return i
		}
	}
	return -1
}

// lastSelectable returns the index of the last selectable row (-1 if none).
func (fleetPage *fleetPage) lastSelectable() int {
	for i := len(fleetPage.rows) - 1; i >= 0; i-- {
		if fleetPage.rows[i].selectable() {
			return i
		}
	}
	return -1
}

// openArmadaSelect opens the armada dropdown with the cursor on the current
// connection, refreshing the remotes' status indicators.
func (fleetPage *fleetPage) openArmadaSelect(m *model) tea.Cmd {
	fleetPage.mode = viewArmadaSelect
	fleetPage.armadaDialogRow = 0
	for i, e := range m.armadaEntries() {
		if e.current {
			fleetPage.armadaDialogRow = i
			break
		}
	}
	return m.pingAllArmadaCmd()
}

// moveCursor moves the cursor by delta rows, wrapping around and
// skipping rows the cursor may not rest on (e.g. instance tag lines).
func (fleetPage *fleetPage) moveCursor(delta int) {
	n := len(fleetPage.rows)
	if n == 0 || delta == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
		delta = -delta
	}
	for ; delta > 0; delta-- {
		for range n {
			fleetPage.cursor = (fleetPage.cursor + step + n) % n
			if fleetPage.rows[fleetPage.cursor].selectable() {
				break
			}
		}
	}
}

// moveCursorToInstance moves the cursor to the next (delta > 0) or previous
// (delta < 0) instance row, wrapping around. If the row list contains no
// instance rows, the cursor is left unchanged.
func (fleetPage *fleetPage) moveCursorToInstance(delta int) {
	n := len(fleetPage.rows)
	if n == 0 || delta == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	for range n {
		fleetPage.cursor = (fleetPage.cursor + step + n) % n
		if fleetPage.rows[fleetPage.cursor].kind == rowInstance {
			return
		}
	}
}

// currentFleetName returns the fleet name for the row at the cursor.
func (fleetPage *fleetPage) currentFleetName() string {
	r := fleetPage.currentRow()
	if r == nil || r.kind == rowSettings || r.kind == rowLeaveFocus {
		return ""
	}
	return r.fleetName
}

// selectedInstance returns the fleet and instance when the cursor is
// on an instance row.
func (fleetPage *fleetPage) selectedInstance(m *model) (*fleet.Fleet, *fleet.Instance) {
	r := fleetPage.currentRow()
	if r == nil || r.kind != rowInstance || r.instance == nil {
		return nil, nil
	}
	f := m.st.Fleets[r.fleetName]
	return f, r.instance
}

// selectedSession returns the fleet, instance, and session name when
// the cursor is on a session row.
func (fleetPage *fleetPage) selectedSession(m *model) (*fleet.Fleet, *fleet.Instance, string) {
	r := fleetPage.currentRow()
	if r == nil || r.kind != rowSession {
		return nil, nil, ""
	}
	f := m.st.Fleets[r.fleetName]
	return f, r.instance, r.sessionName
}

// ===========================================
// Normal Mode Update
// ===========================================

// updateNormal handles keyboard input in the default fleet list mode.
func (fleetPage *fleetPage) updateNormal(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		m.message = ""

		// The Armada selector is a virtual nav stop ABOVE the first row. While
		// it holds focus, j/k move off it (k wraps to the bottom row, j drops to
		// the top row) and enter/space/A open its dropdown; the per-row letter
		// actions don't apply, so they're ignored.
		if fleetPage.armadaFocused {
			switch msg.String() {
			case "ctrl+c", "ctrl+q":
				m.quitting = true
				return tea.Quit
			case "q":
				if fleetPage.focusedFleet != "" {
					fleetPage.leaveFocus(m)
					return nil
				}
				m.quitting = true
				return tea.Quit
			case "up", "k":
				fleetPage.armadaFocused = false
				if i := fleetPage.lastSelectable(); i >= 0 {
					fleetPage.cursor = i
				}
			case "down", "j":
				fleetPage.armadaFocused = false
				if i := fleetPage.firstSelectable(); i >= 0 {
					fleetPage.cursor = i
				}
			case "enter", " ", "A":
				return fleetPage.openArmadaSelect(m)
			case "esc":
				// esc leaves focus (like a dialog) when focused — mirroring q —
				// otherwise it just drops the Armada selector focus.
				if fleetPage.focusedFleet != "" {
					fleetPage.leaveFocus(m)
					return nil
				}
				fleetPage.armadaFocused = false
			}
			return nil
		}

		switch msg.String() {
		case "ctrl+c", "ctrl+q":
			m.quitting = true
			return tea.Quit

		case "q", "esc":
			// Focus mode treats q/esc like a dialog: they leave focus rather
			// than quitting. Outside focus mode q quits and esc is a no-op.
			if fleetPage.focusedFleet != "" {
				fleetPage.leaveFocus(m)
				return nil
			}
			if msg.String() == "q" {
				m.quitting = true
				return tea.Quit
			}

		case "up", "k":
			// Up from the top row focuses the Armada selector (one stop above
			// the list); otherwise move within the rows.
			if fleetPage.cursor == fleetPage.firstSelectable() {
				fleetPage.armadaFocused = true
			} else {
				fleetPage.moveCursor(-1)
			}

		case "down", "j":
			// Down from the bottom row wraps up to the Armada selector.
			if fleetPage.cursor == fleetPage.lastSelectable() {
				fleetPage.armadaFocused = true
			} else {
				fleetPage.moveCursor(1)
			}

		// A dedicated key opens the Armada dropdown from any row; the mouse
		// synthesizes the same key when the border label is clicked.
		case "A":
			return fleetPage.openArmadaSelect(m)

		case "shift+up", "K":
			fleetPage.moveCursorToInstance(-1)

		case "shift+down", "J":
			fleetPage.moveCursorToInstance(1)

		case " ", "tab":
			if r := fleetPage.currentRow(); r != nil {
				switch r.kind {
				case rowFleetHeader:
					name := r.fleetName
					fleetPage.collapsed[name] = !fleetPage.collapsed[name]
					fleetPage.buildRows(m)
				case rowInstance:
					if r.instance == nil {
						break
					}
					if r.instance.Status != fleet.StatusRunning {
						m.message = "Instance must be running to view sessions"
						break
					}
					ref := InstanceRef{Fleet: r.fleetName, Instance: r.instance.Name}
					if m.sessionStore.IsExpanded(ref) {
						m.sessionStore.SetExpanded(ref, false)
						fleetPage.buildRows(m)
					} else {
						m.sessionStore.SetExpanded(ref, true)
						// Discovery comes from the server runtime cache (the server
						// polls tmux for all running instances); read it now and
						// rebuild so the session list shows immediately.
						m.refreshSessionsFromRuntime(ref)
						fleetPage.buildRows(m)
					}
				case rowSession, rowNewSession, rowSettings, rowLeaveFocus:
					return fleetPage.handleEnter(m)
				}
			}

		case "r":
			if r := fleetPage.currentRow(); r != nil && r.kind == rowSession {
				fleetPage.mode = viewRenameSession
				fleetPage.dialogFleet = r.fleetName
				fleetPage.dialogInst = r.instance.Name
				fleetPage.dialogSession = r.sessionName
				displayName := r.sessionName
				if r.instance != nil {
					sanitized := SanitizeSessionName(r.instance.Name)
					if gid, ok := parseGroupID(sanitized, r.sessionName); ok {
						displayName = gid
					}
				}
				fleetPage.textInput.SetValue(displayName)
				fleetPage.textInput.Placeholder = "new-session-name"
				fleetPage.textInput.CharLimit = 64
				return fleetPage.activateTextInput()
			}
			m.reload()
			fleetPage.buildRows(m)
			m.message = "Refreshed"

		case "s":
			r := fleetPage.currentRow()
			if r == nil || r.kind != rowInstance || r.instance == nil {
				m.message = "Select an instance"
				break
			}

			key := r.fleetName + "/" + r.instance.Name
			if isTransitional(r.instance.Status) {
				m.message = fmt.Sprintf("Instance %s is %s", key, r.instance.Status)
				break
			}
			if r.instance.Status == fleet.StatusFailed {
				m.message = fmt.Sprintf("Instance %s is failed and cannot be toggled", key)
				break
			}

			// Start/stop run as server jobs. Flip an optimistic in-memory
			// transitional status for the spinner (NOT persisted — the server owns
			// the transition and the persisted status); operationDoneMsg reload()s
			// the authoritative result.
			fleetName, instName := r.fleetName, r.instance.Name
			var cmd tea.Cmd
			if r.instance.Status == fleet.StatusRunning {
				r.instance.Status = fleet.StatusStopping
				cmd = stopInstanceCmd(fleetName, instName)
			} else if r.instance.Status == fleet.StatusStopped {
				r.instance.Status = fleet.StatusStarting
				cmd = startInstanceCmd(fleetName, instName)
			}
			fleetPage.buildRows(m)
			return cmd

		case "d":
			r := fleetPage.currentRow()
			if r == nil || r.kind == rowSettings || r.kind == rowLeaveFocus || r.kind == rowNewSession {
				break
			}
			if r.kind == rowSession {
				fleetPage.dialogFleet = r.fleetName
				fleetPage.dialogInst = r.instance.Name
				fleetPage.dialogSession = r.sessionName
				fleetPage.dialogGroupID = r.groupID
				fleetPage.mode = viewConfirmDeleteSession
				break
			}
			fleetPage.dialogFleet = r.fleetName
			if r.kind == rowFleetHeader {
				fleetPage.dialogInst = ""
			} else if r.instance != nil {
				fleetPage.dialogInst = r.instance.Name
			} else {
				break
			}
			fleetPage.mode = viewConfirmDelete

		case "a":
			r := fleetPage.currentRow()
			if r == nil {
				m.message = "No fleet selected"
				break
			}
			if r.kind == rowInstance || r.kind == rowSession || r.kind == rowNewSession {
				instance := r.instance
				if instance == nil {
					break
				}
				if instance.Status != fleet.StatusRunning {
					m.message = "Instance must be running to create sessions"
					break
				}
				return fleetPage.openCreateSessionDialog(m, r.fleetName, instance)
			}
			fleetName := fleetPage.currentFleetName()
			if fleetName == "" {
				m.message = "No fleet selected"
				break
			}
			m.toolStatus = deps.CheckTools()
			available := fleetPage.availableBackendTypes(m)
			if len(available) == 0 {
				m.message = "No deploy targets available – install devcontainer or coder CLI"
				break
			}
			fleetPage.mode = viewAddInstance
			fleetPage.dialogFleet = fleetName
			fleetPage.dialogBackend = available[0]
			if m.config != nil {
				preferred := fleet.BackendType(m.config.DefaultBackend)
				for _, backendType := range available {
					if backendType == preferred {
						fleetPage.dialogBackend = preferred
						break
					}
				}
			}
			fleetPage.dialogColor = instanceColorWhite
			fleetPage.dialogRow = addInstanceRowName
			fleetPage.dialogEditing = false
			fleetPage.dialogFieldActive = false
			fleetPage.textInput.SetValue("")
			fleetPage.textInput.Placeholder = "instance-name"
			fleetPage.textInput.CharLimit = 64
			fleetPage.branchInput.SetValue("")
			fleetPage.branchInput.Placeholder = "default branch"
			fleetPage.branchInput.CharLimit = 128
			return fleetPage.activateAddInstanceField()

		case "n":
			fleetPage.mode = viewAddFleet
			fleetPage.textInput.SetValue("")
			fleetPage.textInput.Placeholder = "git@github.com:org/repo.git"
			fleetPage.textInput.CharLimit = 256
			return fleetPage.activateTextInput()

		case "pgup", "pgdown":
			if m.inHostTmux && fleetPage.splitRef.Valid() && !fleetPage.activeGroup.Empty() {
				return fleetPage.cycleSessionGroup(m, msg.String() == "pgup")
			}

		case "enter":
			return fleetPage.handleEnter(m)

		case "e":
			if r := fleetPage.currentRow(); r != nil && r.kind == rowFleetHeader {
				return fleetPage.openEditFleetDialog(m)
			}
			return fleetPage.openEditInstanceDialog(m)

		case "o":
			_, instance := fleetPage.selectedInstance(m)
			if instance == nil {
				m.message = "Select an instance"
				break
			}
			shellCmd := freshShellCommand(m.config)
			cmd, err := attachExecCmd(fleetPage.currentFleetName(), instance.Name, shellCmd)
			if err != nil {
				m.message = fmt.Sprintf("Could not open terminal: %v", err)
				break
			}
			if err := openInTerminal(cmd.Args); err != nil {
				m.message = fmt.Sprintf("Could not open terminal: %v", err)
			} else {
				m.message = fmt.Sprintf("Opened terminal for %s", instance.GetDisplayName())
			}

		case "c":
			_, instance := fleetPage.selectedInstance(m)
			if instance == nil {
				m.message = "Select an instance"
				break
			}
			r := fleetPage.rows[fleetPage.cursor]
			var codeCmd *exec.Cmd
			// Each backend opens VS Code with a local CLI; no container access is
			// needed, so these are computed client-side (no internal/backend) —
			// the editor URI/args are pure and small enough to inline.
			switch instance.Backend {
			case fleet.BackendCoder:
				codeCmd = exec.Command("coder", "open", "vscode", instance.ContainerID)
			case fleet.BackendCodespaces:
				codeCmd = exec.Command("gh", "codespace", "code", "-c", instance.ContainerID)
			default:
				// Devcontainer: VS Code's dev-container remote URI (hex of the host
				// workspace path + the in-container /workspaces/<project> folder).
				uri := fmt.Sprintf("vscode-remote://dev-container+%s/workspaces/%s",
					hex.EncodeToString([]byte(instance.WorkspaceDir)), r.fleetName)
				codeCmd = exec.Command("code", "--folder-uri", uri)
			}
			if codeCmd != nil {
				if err := codeCmd.Run(); err != nil {
					m.message = fmt.Sprintf("VS Code error: %v", err)
				} else {
					m.message = fmt.Sprintf("Opened VS Code for %s", instance.GetDisplayName())
				}
			}

		case "C":
			_, instance := fleetPage.selectedInstance(m)
			if instance == nil {
				m.message = "Select an instance"
				break
			}
			// Only the devcontainer backend supports clone (coder/codespaces don't);
			// "" defaults to devcontainer. Pure check — no backend construction.
			if instance.Backend != fleet.BackendDevcontainer && instance.Backend != "" {
				m.message = fmt.Sprintf("Clone not supported by %s backend", instance.Backend)
				break
			}
			if instance.ContainerID == "" {
				m.message = "Instance has not finished provisioning; nothing to clone yet"
				break
			}
			fleetPage.mode = viewCloneInstance
			fleetPage.dialogFleet = fleetPage.currentFleetName()
			fleetPage.dialogInst = instance.Name
			fleetPage.textInput.SetValue("")
			fleetPage.textInput.Placeholder = "destination-name"
			fleetPage.textInput.CharLimit = 64
			return fleetPage.activateTextInput()

		case "R":
			_, instance := fleetPage.selectedInstance(m)
			if instance == nil {
				m.message = "Select an instance"
				break
			}
			// Coder workspaces have no rebuild primitive; devcontainer ("" defaults
			// to devcontainer) and codespaces do. Pure check — no backend construction.
			if instance.Backend == fleet.BackendCoder {
				m.message = fmt.Sprintf("Rebuild not supported by %s backend", instance.Backend)
				break
			}
			if isTransitional(instance.Status) {
				m.message = fmt.Sprintf("Instance %s/%s is %s", fleetPage.currentFleetName(), instance.Name, instance.Status)
				break
			}
			if instance.ContainerID == "" {
				m.message = "Instance has not finished provisioning; nothing to rebuild yet"
				break
			}
			fleetPage.mode = viewConfirmRebuild
			fleetPage.dialogFleet = fleetPage.currentFleetName()
			fleetPage.dialogInst = instance.Name

		case "b":
			_, instance := fleetPage.selectedInstance(m)
			if instance == nil {
				m.message = "Select an instance"
				break
			}
			if instance.Status != fleet.StatusRunning {
				m.message = "Instance must be running to open browser"
				break
			}
			return fleetPage.beginBrowserOpen(m, instance, fleetPage.currentFleetName())

		case "t":
			_, instance := fleetPage.selectedInstance(m)
			if instance == nil {
				m.message = "Select an instance"
				break
			}
			fleetPage.mode = viewTagInstance
			fleetPage.dialogFleet = fleetPage.currentFleetName()
			fleetPage.dialogInst = instance.Name
			fleetPage.textInput.SetValue(instance.Tag)
			fleetPage.textInput.Placeholder = "short description"
			fleetPage.textInput.CharLimit = 128
			fleetPage.deactivateTextInput()
			return nil

		case "l":
			_, instance := fleetPage.selectedInstance(m)
			if instance == nil {
				m.message = "Select an instance"
				break
			}
			r := fleetPage.rows[fleetPage.cursor]
			return execProcess(
				logsCommand(r.fleetName, instance),
				func(err error) tea.Msg { return execDoneMsg{err} },
			)

		case "f":
			// Focus mode hides every fleet but the selected one. Toggles off if
			// already focused; works from any row belonging to a fleet.
			if fleetPage.focusedFleet != "" {
				fleetPage.leaveFocus(m)
				break
			}
			name := fleetPage.currentFleetName()
			if name == "" {
				m.message = "Select a fleet to focus"
				break
			}
			fleetPage.enterFocus(m, name)

		case "p":
			_, instance := fleetPage.selectedInstance(m)
			if instance == nil {
				m.message = "Select an instance"
				break
			}
			if instance.Status != fleet.StatusRunning {
				m.message = fmt.Sprintf("Instance must be running to port-forward (status: %s)", instance.Status)
				break
			}
			fleetPage.mode = viewPortForward
			fleetPage.dialogFleet = fleetPage.currentFleetName()
			fleetPage.dialogInst = instance.Name
			fleetPage.pfCursor = 0
			fleetPage.textInput.SetValue("")
			fleetPage.textInput.Placeholder = "local:remote (e.g. 8080:80)"
			fleetPage.textInput.CharLimit = 11
			fleetPage.deactivateTextInput()
			return nil
		}

	case execDoneMsg:
		m.reload()
		fleetPage.buildRows(m)
		if msg.err != nil {
			m.message = fmt.Sprintf("Command error: %v", msg.err)
		}
	}

	return nil
}

// ===========================================
// Enter Handler
// ===========================================

// handleEnter executes the enter/e/space action for the current row.
func (fleetPage *fleetPage) handleEnter(m *model) tea.Cmd {
	r := fleetPage.currentRow()
	if r == nil {
		return nil
	}

	switch r.kind {
	case rowSettings:
		m.toolStatus = deps.CheckTools()
		return m.ChangeRoute(routeSettings)

	case rowLeaveFocus:
		fleetPage.leaveFocus(m)

	case rowFleetHeader:
		name := r.fleetName
		fleetPage.collapsed[name] = !fleetPage.collapsed[name]
		fleetPage.buildRows(m)

	case rowNewSession:
		return fleetPage.openCreateSessionDialog(m, r.fleetName, r.instance)

	case rowSession:
		instance := r.instance
		sessionName := r.sessionName
		groupID := r.groupID
		sessRef := InstanceRef{Fleet: r.fleetName, Instance: instance.Name}
		m.sessionStore.SetLastActive(sessRef, lastSession{sessionName: sessionName, groupID: groupID})
		if m.inHostTmux {
			if fleetPage.restoreInProgress() {
				m.message = "Pane group restore already in progress"
				return nil
			}
			if fleetPage.splitPaneID != "" && !splitOpen() {
				unbindHostSplitKeys()
				fleetPage.clearSplit()
			}
			rowGroup := ActiveGroup{Ref: sessRef, GroupID: groupID}
			// Same instance + same group → toggle split closed.
			if fleetPage.splitPaneID != "" && fleetPage.splitRef == sessRef && groupID != "" && fleetPage.activeGroup == rowGroup {
				fleetPage.saveCurrentGroupLayout(m)
				killAllSplitPanes()
				unbindHostSplitKeys()
				fleetPage.clearSplit()
				return nil
			}
			if fleetPage.splitPaneID != "" && !fleetPage.activeGroup.Empty() {
				fleetPage.saveCurrentGroupLayout(m)
				killAllSplitPanes()
			}
			fleetPage.activeGroup = rowGroup
			if groupID != "" && isGroupedSession(SanitizeSessionName(instance.Name), sessionName) {
				return fleetPage.restoreGroupCmd(m, r.fleetName, instance, groupID)
			}
			cols, rows := tmuxWindowSize()
			cols = cols * 70 / 100
			shellCmd := ShellCommandForSession(m.config, sessionName, cols, rows, true)
			cmd, err := attachExecCmd(r.fleetName, instance.Name, shellCmd)
			if err != nil {
				m.message = fmt.Sprintf("Could not open session: %v", err)
				return nil
			}
			return splitPaneCmd(fleetPage.splitPaneID, sessRef, sessionName, groupID, cmd)
		}
		shellCmd := ShellCommandForSession(m.config, sessionName, m.width, m.height, false)
		cmd, err := attachExecCmd(r.fleetName, instance.Name, shellCmd)
		if err != nil {
			m.message = fmt.Sprintf("Could not open session: %v", err)
			return nil
		}
		banner := renderGradient(nameToBanner(instance.GetDisplayName()))
		banner += "\n  " + dimStyle.Render("ctrl+q/ctrl+o to detach (session persists)")
		return execProcess(
			execWithBannerCmd(banner, cmd),
			func(err error) tea.Msg { return execDoneMsg{err} },
		)

	case rowInstance:
		_, instance := fleetPage.selectedInstance(m)
		if instance == nil {
			break
		}
		instFleetName := r.fleetName
		instRef := InstanceRef{Fleet: instFleetName, Instance: instance.Name}
		if m.inHostTmux {
			if fleetPage.restoreInProgress() {
				m.message = "Pane group restore already in progress"
				return nil
			}
			if fleetPage.splitPaneID != "" && !splitOpen() {
				unbindHostSplitKeys()
				fleetPage.clearSplit()
			}
			if fleetPage.splitPaneID != "" && fleetPage.splitRef == instRef {
				fleetPage.saveCurrentGroupLayout(m)
				killAllSplitPanes()
				unbindHostSplitKeys()
				fleetPage.clearSplit()
				return nil
			}
			return fleetPage.openInstanceSession(m, instFleetName, instance)
		}

		sessionName := SanitizeSessionName(instance.Name)
		if last, ok := m.sessionStore.LastActive(instRef); ok {
			sessionName = last.sessionName
		}
		m.sessionStore.SetLastActive(instRef, lastSession{sessionName: sessionName})

		shellCmd := ShellCommandForSession(m.config, sessionName, m.width, m.height, false)
		cmd, err := attachExecCmd(instFleetName, instance.Name, shellCmd)
		if err != nil {
			m.message = fmt.Sprintf("Could not open session: %v", err)
			return nil
		}
		banner := renderGradient(nameToBanner(instance.GetDisplayName()))
		banner += "\n  " + dimStyle.Render("ctrl+q/ctrl+o to detach (session persists)")
		return execProcess(
			execWithBannerCmd(banner, cmd),
			func(err error) tea.Msg { return execDoneMsg{err} },
		)
	}

	return nil
}

// ===========================================
// Help Keys
// ===========================================

// renderArmadaBorder draws the list box's top border line with the Armada
// selector embedded: ╭─ Armada [ local ] ───╮. The line is hand-composed to
// the box's exact rendered width (lipgloss has no border-title API) in the
// box's border color; the label's column span is recorded for mouse
// hit-testing. Falls back to a plain border when the box is too narrow.
func (fleetPage *fleetPage) renderArmadaBorder(m *model, width int) string {
	borderStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("63"))

	name := m.armadaCurrentDisplay()
	frame := " Armada [  ] "
	// Corners + 2 leading dashes + at least 2 trailing dashes must survive.
	maxName := width - 6 - lipgloss.Width(frame)
	if maxName < 1 {
		fleetPage.armadaX0, fleetPage.armadaX1 = -1, -1
		return borderStyle.Render("╭" + strings.Repeat("─", max(0, width-2)) + "╮")
	}
	if lipgloss.Width(name) > maxName {
		name = ansi.Truncate(name, maxName, "…")
	}
	// labelWidth drives layout + mouse hit-testing, so measure the PLAIN text;
	// the ANSI styling applied below doesn't change the visible width.
	label := " Armada [ " + name + " ] "
	labelWidth := lipgloss.Width(label)

	// "Armada" wears the same light-cyan→deep-blue gradient as the "fleet" logo
	// header. The brackets + current-connection name follow the border colour,
	// switching to the selection highlight while the selector is focused / open.
	rest := " [ " + name + " ] "
	restStyle := borderStyle
	if fleetPage.armadaFocused || fleetPage.mode == viewArmadaSelect {
		restStyle = selectedStyle
	}
	styledLabel := " " + renderGradient("Armada") + restStyle.Render(rest)

	fleetPage.armadaX0 = 3
	fleetPage.armadaX1 = 3 + labelWidth
	rightDashes := max(0, width-4-labelWidth)
	return borderStyle.Render("╭──") + styledLabel + borderStyle.Render(strings.Repeat("─", rightDashes)+"╮")
}

// contextualHelpKeys returns the footer hints for the current row, adding a
// "f: focus" discovery hint on any fleet row. (Focus mode itself hides the help
// bar, so there are no in-focus hints to render.)
func (fleetPage *fleetPage) contextualHelpKeys(m *model) []string {
	keys := fleetPage.contextualHelpKeysBase(m)
	// The Armada selector swallows its own keys, so 'f' does nothing there.
	if !fleetPage.armadaFocused && fleetPage.currentFleetName() != "" {
		keys = insertHelpHintBefore(keys, "q: quit", "f: focus")
	}
	return keys
}

func (fleetPage *fleetPage) contextualHelpKeysBase(m *model) []string {
	if fleetPage.armadaFocused {
		return []string{"enter/space: switch armada", "j/k: navigate", "q: quit"}
	}
	r := fleetPage.currentRow()
	if r == nil {
		return withArmadaHint([]string{"n: new fleet", "q: quit"})
	}

	switch r.kind {
	case rowFleetHeader:
		return withArmadaHint([]string{
			"j/k: navigate", "space/enter: expand/collapse", "e: edit fleet",
			"a: add instance", "n: new fleet", "d: delete fleet", "r: refresh", "q: quit",
		})

	case rowInstance:
		keys := []string{"j/k: navigate"}
		if r.instance != nil {
			switch {
			case r.instance.Status == fleet.StatusRunning:
				keys = append(keys,
					"space: show sessions", "enter: open shell", "e: edit",
					"s: stop", "a: new session", "d: delete", "t: tag",
					"p: port-forward", "b: browser", "c: code", "C: clone", "R: rebuild", "o: terminal", "l: logs",
					"r: refresh", "q: quit",
				)
			case r.instance.Status == fleet.StatusStopped:
				keys = append(keys,
					"enter: open shell", "e: edit", "s: start",
					"a: new session", "d: delete", "t: tag", "R: rebuild", "r: refresh", "q: quit",
				)
			case r.instance.Status == fleet.StatusFailed:
				keys = append(keys, "d: delete", "r: refresh", "q: quit")
			case isTransitional(r.instance.Status):
				keys = append(keys, "r: refresh", "q: quit")
			default:
				keys = append(keys, "r: refresh", "q: quit")
			}
		}
		return withArmadaHint(keys)

	case rowSession:
		keys := []string{
			"j/k: navigate", "space/enter/e: connect",
			"a: new session", "d: delete session", "r: rename", "q: quit",
		}
		if m.inHostTmux && fleetPage.splitRef.Valid() && !fleetPage.activeGroup.Empty() {
			keys = append(keys[:len(keys)-1], "pgup/pgdn: cycle groups", "q: quit")
		}
		return withArmadaHint(keys)

	case rowNewSession:
		return withArmadaHint([]string{
			"j/k: navigate", "space/enter/e: create session",
			"a: new session", "q: quit",
		})

	case rowSettings:
		return withArmadaHint([]string{
			"j/k: navigate", "space/enter/e: open settings",
			"n: new fleet", "q: quit",
		})
	}

	return withArmadaHint([]string{"q: quit"})
}

// insertHelpHintBefore inserts item just before the first occurrence of anchor,
// or appends it when anchor is absent.
func insertHelpHintBefore(keys []string, anchor, item string) []string {
	for i, k := range keys {
		if k == anchor {
			out := make([]string, 0, len(keys)+1)
			out = append(out, keys[:i]...)
			out = append(out, item)
			out = append(out, keys[i:]...)
			return out
		}
	}
	return append(keys, item)
}

// withArmadaHint inserts the global "A: armada" hint just before a trailing
// "q: quit" (the `A` key opens the armada selector from every fleet-list row),
// unless it's already present.
func withArmadaHint(keys []string) []string {
	for _, k := range keys {
		if k == "A: armada" {
			return keys
		}
	}
	if n := len(keys); n > 0 && keys[n-1] == "q: quit" {
		out := make([]string, 0, n+1)
		out = append(out, keys[:n-1]...)
		out = append(out, "A: armada", "q: quit")
		return out
	}
	return append(keys, "A: armada")
}

// ===========================================
// View
// ===========================================

// viewFleetList renders the fleet list page.
func (fleetPage *fleetPage) viewFleetList(m *model) string {
	var b strings.Builder

	logo := "" +
		"  __ _         _\n" +
		" / _| |___ ___| |_\n" +
		"|  _| / -_) -_)  _|\n" +
		"|_| |_\\___\\___|\\___|"
	rendered := renderGradient(logo)
	if ind := remoteIndicator(m); ind != "" {
		// Float the signal glyph up-and-right off the top of the "t" by
		// appending it to the logo's first line.
		lines := strings.Split(rendered, "\n")
		lines[0] = lines[0] + "    " + ind
		rendered = strings.Join(lines, "\n")
	}
	b.WriteString(rendered)
	if chain := versionChain(m); chain != "" {
		b.WriteString(" " + dimStyle.Render(chain))
	}
	if m.updateAvailable != "" {
		b.WriteString("  " + updateStyle.Render(fmt.Sprintf("A new version: %s is available ⚡ Settings to update", m.updateAvailable)))
	}
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(errorStyle.Render(fmt.Sprintf("Error: %v", m.err)))
		b.WriteString("\n")
	}

	var listContent strings.Builder

	if m.st == nil || len(m.st.Fleets) == 0 {
		listContent.WriteString(dimStyle.Render("  No instances. Press 'a' to create one, or use 'fleet up <name>'."))
		listContent.WriteString("\n")
	}

	for i, r := range fleetPage.rows {
		// While the Armada selector holds focus, no row shows the cursor.
		isSelected := i == fleetPage.cursor && !fleetPage.armadaFocused
		cursor := "  "
		if isSelected {
			cursor = cursorStyle.Render("> ")
		}

		if r.kind == rowFleetHeader {
			arrow := "▼ "
			style := fleetExpandedStyle
			if fleetPage.collapsed[r.fleetName] {
				arrow = "▶ "
				style = fleetCollapsedStyle
			}

			count := 0
			if f, ok := m.st.Fleets[r.fleetName]; ok {
				count = len(f.Instances)
			}
			suffix := dimStyle.Render(fmt.Sprintf(" (%d)", count))

			if isSelected {
				listContent.WriteString(fmt.Sprintf("%s%s%s",
					cursor,
					selectedStyle.Render(arrow+r.fleetName),
					suffix,
				))
			} else {
				listContent.WriteString(fmt.Sprintf("%s%s%s%s",
					cursor,
					style.Render(arrow),
					style.Render(r.fleetName),
					suffix,
				))
			}
			listContent.WriteString("\n")
		} else if r.kind == rowSession {
			icon := "○"
			style := sessionStyle
			displayGroup := fleetPage.activeGroup
			if !fleetPage.pendingGroup.Empty() {
				displayGroup = fleetPage.pendingGroup
			}
			rowGroup := ActiveGroup{
				Ref:     InstanceRef{Fleet: r.fleetName, Instance: r.instance.Name},
				GroupID: r.groupID,
			}
			if r.groupID != "" && rowGroup == displayGroup {
				icon = "●"
				style = sessionActiveStyle
			}
			var label string
			if r.groupSize > 1 {
				label = fmt.Sprintf("%s %s (%d panes)", icon, r.groupID, r.groupSize)
			} else if r.groupID != "" && isGroupedSession(SanitizeSessionName(r.instance.Name), r.sessionName) {
				label = fmt.Sprintf("%s %s", icon, r.groupID)
			} else {
				label = fmt.Sprintf("%s %s", icon, r.sessionName)
			}
			if isSelected {
				listContent.WriteString(fmt.Sprintf("%s        %s", cursor, selectedStyle.Render(label)))
			} else {
				listContent.WriteString(fmt.Sprintf("%s        %s", cursor, style.Render(label)))
			}
			listContent.WriteString("\n")

		} else if r.kind == rowNewSession {
			label := "+ new session"
			if isSelected {
				listContent.WriteString(fmt.Sprintf("%s        %s", cursor, selectedStyle.Render(label)))
			} else {
				listContent.WriteString(fmt.Sprintf("%s        %s", cursor, newSessionStyle.Render(label)))
			}
			listContent.WriteString("\n")

		} else if r.kind == rowInstance {
			instance := r.instance

			transitional := isTransitional(instance.Status)
			var status string
			if transitional {
				status = strings.TrimRight(m.spinner.View(), "\n") + " " + statusCreatingStyle.Render(string(instance.Status))
			} else {
				status = renderStatus(instance.Status)
			}

			ref := InstanceRef{Fleet: r.fleetName, Instance: instance.Name}
			arrow := "  "
			if instance.Status == fleet.StatusRunning {
				if m.sessionStore.IsExpanded(ref) {
					arrow = "▼ "
				} else {
					arrow = "▶ "
				}
			}

			// Single switch derives BOTH the left-of-name throbber
			// and the right-of-status agentStr. Colors mirror the
			// right indicator: green animated pulse while working,
			// yellow static ○ while the agent is alive but waiting,
			// grey ○ when the agent is absent (or instance not
			// running). agentStr is consumed below in the
			// non-transitional render branch.
			throbber := agentOffStyle.Render("○")
			agentStr := ""
			if instance.Status == fleet.StatusRunning {
				// Live agent state comes from the server's runtime sidecar
				// (P2 Step 7). A missing entry / UNSPECIFIED activity renders as
				// "○ idle" (same as NOT_RUNNING), so a running row shows idle
				// immediately, before the first runtime push lands.
				rt := m.runtime[rtKey(r.fleetName, instance.Name)]
				label := agentToolLabelProto(rt.GetAgentTool())
				switch rt.GetAgentActivity() {
				case fleetgrpc.AgentActivity_AGENT_ACTIVITY_WORKING:
					agentStr = agentWorkingStyle.Render(fmt.Sprintf("  ▶ %s", label))
					if len(m.agentSpinner.Spinner.Frames) > 0 {
						throbber = strings.TrimRight(m.agentSpinner.View(), "\n")
					} else {
						throbber = agentWorkingStyle.Render("✻")
					}
				case fleetgrpc.AgentActivity_AGENT_ACTIVITY_WAITING:
					agentStr = agentWaitingStyle.Render(fmt.Sprintf("  ⏸ %s", label))
					throbber = agentWaitingStyle.Render("○")
				default:
					agentStr = agentOffStyle.Render("  ○ idle")
				}
			}

			nameRaw := fmt.Sprintf("%-22s", instance.GetDisplayName())
			var arrowStyled, nameStyled string
			switch {
			case isSelected && instanceColorHasCustom(instance.Color):
				colorStyle := instanceColorStyle(instance.Color).Bold(true)
				arrowStyled = colorStyle.Render(arrow)
				nameStyled = colorStyle.Render(nameRaw)
			case isSelected:
				arrowStyled = selectedStyle.Render(arrow)
				nameStyled = selectedStyle.Render(nameRaw)
			case instanceColorHasCustom(instance.Color):
				colorStyle := instanceColorStyle(instance.Color)
				arrowStyled = colorStyle.Render(arrow)
				nameStyled = colorStyle.Render(nameRaw)
			default:
				arrowStyled = arrow
				nameStyled = nameRaw
			}
			paddedName := arrowStyled + throbber + " " + nameStyled

			backendIcon := "⬡"
			switch instance.Backend {
			case fleet.BackendCoder:
				backendIcon = "⌨"
			case fleet.BackendCodespaces:
				backendIcon = "⏣"
			}
			branchItem := ""
			if branch := resolveWorkspaceBranch(instance.WorkspaceDir); branch != "" {
				branchItem = dimStyle.Render("  " + branch + " " + backendIcon)
			} else {
				branchItem = dimStyle.Render("  " + backendIcon)
			}

			var line string
			if transitional {
				line = fmt.Sprintf("%s    %s %s%s",
					cursor, paddedName, status, branchItem,
				)
			} else {
				statsStr := ""
				if s := m.runtime[rtKey(r.fleetName, instance.Name)].GetStats(); s != nil {
					statsStr = dimStyle.Render(fmt.Sprintf("  %4.0f mcpu  %6.1f MB", s.GetCpuMillicores(), s.GetMemoryMb()))
				}

				line = fmt.Sprintf("%s    %s %s%s%s%s",
					cursor, paddedName, status, agentStr, statsStr, branchItem,
				)

				pfKey := r.fleetName + "/" + instance.Name
				if pfLabel := m.portForwards.FormatLabels(pfKey); pfLabel != "" {
					line += portForwardStyle.Render("  ⇄ " + pfLabel)
				}
			}

			if maxW := m.width - 4; maxW > 0 && lipgloss.Width(line) > maxW {
				line = ansi.Truncate(line, maxW-1, "…")
			}

			listContent.WriteString(line)
			listContent.WriteString("\n")
		} else if r.kind == rowInstanceTag {
			line := fmt.Sprintf("%s        %s", cursor, dimStyle.Render("# "+r.instance.Tag))
			if maxW := m.width - 4; maxW > 0 && lipgloss.Width(line) > maxW {
				line = ansi.Truncate(line, maxW-1, "…")
			}
			listContent.WriteString(line)
			listContent.WriteString("\n")
		} else {
			label := "settings"
			if r.kind == rowLeaveFocus {
				label = "[ leave focus ]"
			}
			if isSelected {
				listContent.WriteString(fmt.Sprintf("%s%s", cursor, selectedStyle.Render(label)))
			} else {
				listContent.WriteString(fmt.Sprintf("%s%s", cursor, dimStyle.Render(label)))
			}
			listContent.WriteString("\n")
		}
	}

	boxContent := strings.TrimRight(listContent.String(), "\n")
	// The box's own top border is dropped and replaced by a hand-composed
	// line carrying the Armada selector (lipgloss has no border-title API):
	// ╭─ Armada [ local ] ────────╮
	box := listBox.BorderTop(false)
	if m.width > 0 {
		box = box.Width(m.width - 2)
	}
	renderedBox := box.Render(boxContent)
	boxWidth := lipgloss.Width(strings.SplitN(renderedBox, "\n", 2)[0])
	fleetPage.armadaY = strings.Count(b.String(), "\n")
	b.WriteString(fleetPage.renderArmadaBorder(m, boxWidth))
	b.WriteString("\n")
	// Record where rows[0] will land on screen so mouse clicks can map
	// Y → row index. The cursor is at line `newlines` after consuming
	// `b` so far (the armada line above replaced the box's top border);
	// +emptyMsgLines skips the "No instances" line that precedes the
	// (settings-only) rows when no fleets exist.
	emptyMsgLines := 0
	if m.st == nil || len(m.st.Fleets) == 0 {
		emptyMsgLines = 1
	}
	fleetPage.listRowY = strings.Count(b.String(), "\n") + emptyMsgLines
	b.WriteString(renderedBox)
	b.WriteString("\n")

	var totalCPU float64
	var totalMem float64
	statsCount := 0
	for _, rt := range m.runtime {
		if s := rt.GetStats(); s != nil {
			totalCPU += s.GetCpuMillicores()
			totalMem += s.GetMemoryMb()
			statsCount++
		}
	}
	if statsCount > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  Total: %.0f mcpu  %.1f MB", totalCPU, totalMem)))
		b.WriteString("\n")
	}

	// Dialog overlay
	switch fleetPage.mode {
	case viewConfirmDelete:
		b.WriteString("\n")
		var title, body string
		if fleetPage.dialogInst == "" {
			count := 0
			if f, ok := m.st.Fleets[fleetPage.dialogFleet]; ok {
				count = len(f.Instances)
			}
			title = "Delete fleet"
			body = fmt.Sprintf("Remove fleet %s and all %d instance(s)? This will stop all containers and delete all workspaces.", fleetPage.dialogFleet, count)
		} else {
			title = "Delete instance"
			body = fmt.Sprintf("Remove %s/%s? This will stop the container and delete the workspace.", fleetPage.dialogFleet, fleetPage.dialogInst)
		}
		dialog := fmt.Sprintf(
			"%s\n\n%s\n\n%s",
			dialogTitle.Render(title),
			dialogLabel.Render(body),
			dialogHint.Render("[y] Yes  [n/q/esc] No"),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewConfirmRebuild:
		b.WriteString("\n")
		body := fmt.Sprintf("Rebuild %s/%s? This recreates the container from its devcontainer config. Your workspace — the git checkout and any uncommitted changes — is preserved.", fleetPage.dialogFleet, fleetPage.dialogInst)
		dialog := fmt.Sprintf(
			"%s\n\n%s\n\n%s",
			dialogTitle.Render("Rebuild instance"),
			dialogLabel.Render(body),
			dialogHint.Render("[y] Yes  [n/q/esc] No"),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewConfirmDeleteFleetWarn:
		b.WriteString("\n")
		count := 0
		if f, ok := m.st.Fleets[fleetPage.dialogFleet]; ok {
			count = len(f.Instances)
		}
		warnDialog := fmt.Sprintf(
			"%s\n\n%s\n\n%s\n\n%s",
			warnBanner.Render("  !! WARNING !!  "),
			dialogLabel.Render(fmt.Sprintf(
				"You are about to destroy fleet %s with %d running instance(s).\nAll containers will be stopped and all workspace data will be permanently deleted.",
				fleetPage.dialogFleet, count,
			)),
			errorStyle.Render("This action cannot be undone."),
			dialogHint.Render("[y] Confirm destroy  [n/q/esc] Cancel"),
		)
		b.WriteString(warnBox.Render(warnDialog))
		b.WriteString("\n")

	case viewAddInstance:
		b.WriteString("\n")
		backendType := fleetPage.dialogBackend
		if backendType == "" {
			backendType = fleet.BackendDevcontainer
		}
		colorName := fleetPage.dialogColor
		if colorName == "" {
			colorName = instanceColorWhite
		}

		var title, hint, nameField, branchField, deployField string
		if fleetPage.dialogEditing {
			title = "Edit instance"
			if fleetPage.dialogFieldActive {
				hint = "[enter] Save  [esc] Done editing  [ctrl+c] Cancel"
			} else {
				hint = "[j/k] Select  [h/l/space] Cycle color  [shift+tab] Color  [enter] Edit/Save  [q/esc] Cancel"
			}
			nameField = fleetPage.textInput.View()
			branchDisplay := fleetPage.branchInput.Value()
			if branchDisplay == "" {
				branchDisplay = "default"
			}
			branchField = dimStyle.Render(branchDisplay)
			deployField = dimStyle.Render(fmt.Sprintf("[ %s ]", backendTypeLabel(backendType)))
		} else {
			title = "New instance"
			if fleetPage.dialogFieldActive {
				hint = "[enter] Create  [esc] Done editing  [ctrl+c] Cancel"
			} else {
				hint = "[j/k] Select  [h/l/space] Cycle  [shift+tab] Color  [enter] Edit/Create  [q/esc] Cancel"
				if len(fleetPage.availableBackendTypes(m)) > 1 {
					hint = "[j/k] Select  [h/l/space/tab] Cycle  [shift+tab] Color  [enter] Edit/Create  [q/esc] Cancel"
				}
			}
			nameField = fleetPage.textInput.View()
			branchField = fleetPage.branchInput.View()
			deployField = fmt.Sprintf("[ %s ]", backendTypeLabel(backendType))
		}

		rowMarker := func(r int) string {
			if !fleetPage.addInstanceRowEnabled(r) {
				return "  "
			}
			if fleetPage.dialogRow == r {
				return cursorStyle.Render("> ")
			}
			return "  "
		}

		colorPreview := instanceColorStyle(colorName).Render(colorName)
		dialog := fmt.Sprintf(
			"%s\n\n  %s %s\n%s%s %s\n%s%s %s\n%s%s [ %s ]\n%s%s %s\n\n%s",
			dialogTitle.Render(title),
			dialogLabel.Render("Fleet:  "),
			fleetExpandedStyle.Render(fleetPage.dialogFleet),
			rowMarker(addInstanceRowName),
			dialogLabel.Render("Name:   "),
			nameField,
			rowMarker(addInstanceRowBranch),
			dialogLabel.Render("Branch: "),
			branchField,
			rowMarker(addInstanceRowColor),
			dialogLabel.Render("Color:  "),
			colorPreview,
			rowMarker(addInstanceRowDeploy),
			dialogLabel.Render("Deploy: "),
			deployField,
			dialogHint.Render(hint),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewAddFleet:
		b.WriteString("\n")
		dialog := fmt.Sprintf(
			"%s\n\n%s %s\n\n%s",
			dialogTitle.Render("New fleet"),
			dialogLabel.Render("Repo:"),
			fleetPage.textInput.View(),
			dialogHint.Render(fleetPage.textDialogHint("Add")),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewAddFleetInspecting:
		b.WriteString("\n")
		dialog := fmt.Sprintf(
			"%s\n\n%s %s\n\n%s %s\n\n%s",
			dialogTitle.Render("New fleet"),
			dialogLabel.Render("Repo: "),
			fleetExpandedStyle.Render(fleetPage.dialogPendingRepoURL),
			m.spinner.View(),
			dialogLabel.Render("Inspecting for devcontainer.json..."),
			dialogHint.Render("[q/esc] Cancel"),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewAddFleetNoDevcontainer:
		b.WriteString("\n")
		agentName, _, agentErr := devcontainersetup.FindAgent()
		var setupLine string
		if agentErr != nil {
			setupLine = statusCreatingStyle.Render("no agent found") +
				"  " + dimStyle.Render("install claude, codex, gemini, or copilot to use Setup")
		} else {
			setupLine = statusRunningStyle.Render(agentName) +
				"  " + dimStyle.Render("will clone the repo and walk you through configuration")
		}
		dialog := fmt.Sprintf(
			"%s\n\n%s\n\n%s %s\n\n%s\n\n%s\n\n%s",
			warnBanner.Render("  No devcontainer.json found  "),
			dialogLabel.Render(
				"This repository has no .devcontainer/devcontainer.json.\n"+
					"fleet-man needs one before it can provision instances.",
			),
			dialogLabel.Render("Repo:"),
			fleetExpandedStyle.Render(fleetPage.dialogPendingRepoURL),
			dialogLabel.Render("Setup agent: ")+setupLine,
			dialogLabel.Render(
				"[a] Abort — do not add the fleet (default)\n"+
					"[s] Setup — add the fleet now and launch a guided agent to write the devcontainer",
			),
			dialogHint.Render("[a/q/enter/esc] Abort  [s] Setup"),
		)
		b.WriteString(warnBox.Render(dialog))
		b.WriteString("\n")

	case viewEditFleet:
		b.WriteString("\n")
		b.WriteString(dialogBox.Render(fleetPage.renderEditFleet(m)))
		b.WriteString("\n")

	case viewLayoutPreset:
		b.WriteString("\n")
		b.WriteString(dialogBox.Render(fleetPage.renderLayoutPresetDialog()))
		b.WriteString("\n")

	case viewTagInstance:
		b.WriteString("\n")
		dialog := fmt.Sprintf(
			"%s\n\n%s %s\n%s %s\n\n%s",
			dialogTitle.Render("Tag instance"),
			dialogLabel.Render("Instance:"),
			fleetExpandedStyle.Render(fleetPage.dialogFleet+"/"+fleetPage.dialogInst),
			dialogLabel.Render("Tag:     "),
			fleetPage.textInput.View(),
			dialogHint.Render(fleetPage.textDialogHint("Save")),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewPortForward:
		b.WriteString("\n")
		pfKey := fleetPage.dialogFleet + "/" + fleetPage.dialogInst
		fwds := m.portForwards.List(pfKey)

		var fwdLines strings.Builder
		if len(fwds) == 0 {
			fwdLines.WriteString(dimStyle.Render("  No active forwards"))
		} else {
			for i, f := range fwds {
				pfCursor := "  "
				if i == fleetPage.pfCursor {
					pfCursor = cursorStyle.Render("> ")
				}
				fwdLines.WriteString(fmt.Sprintf("%s%s\n",
					pfCursor,
					portForwardStyle.Render(f.Label()),
				))
			}
		}

		dialog := fmt.Sprintf(
			"%s\n\n%s %s\n\n%s\n\n%s %s\n\n%s",
			dialogTitle.Render("Port forwards"),
			dialogLabel.Render("Instance:"),
			fleetExpandedStyle.Render(fleetPage.dialogFleet+"/"+fleetPage.dialogInst),
			strings.TrimRight(fwdLines.String(), "\n"),
			dialogLabel.Render("Add:"),
			fleetPage.textInput.View(),
			dialogHint.Render(fleetPage.portForwardHint()),
		)
		b.WriteString(portForwardBox.Render(dialog))
		b.WriteString("\n")

	case viewCodespacesAuth:
		b.WriteString("\n")
		dialog := fmt.Sprintf(
			"%s\n\n%s\n\n%s\n\n%s",
			warnBanner.Render("  GitHub Auth Required  "),
			dialogLabel.Render(
				"GitHub CLI authentication with the \"codespace\" scope is\n"+
					"required. Press Enter to log in and grant the required scope.",
			),
			dimStyle.Render("gh auth login -h github.com -s codespace"),
			dialogHint.Render("[enter] Authenticate  [q/esc] Cancel"),
		)
		b.WriteString(warnBox.Render(dialog))
		b.WriteString("\n")

	case viewCodespacesMachine:
		b.WriteString("\n")
		dialog := fmt.Sprintf(
			"%s\n\n%s\n\n%s",
			warnBanner.Render("  Machine Type Required  "),
			dialogLabel.Render(
				"GitHub Codespaces requires a machine type but none is\n"+
					"configured. Press Enter to open Settings and set one.",
			),
			dialogHint.Render("[enter] Open Settings  [q/esc] Cancel"),
		)
		b.WriteString(warnBox.Render(dialog))
		b.WriteString("\n")

	case viewCodespacesLimit:
		b.WriteString("\n")
		dialog := fmt.Sprintf(
			"%s\n\n%s\n\n%s",
			warnBanner.Render("  Codespace Limit Reached  "),
			dialogLabel.Render(
				"You have started the maximum number of Codespaces.\n"+
					"Please stop some before creating a new instance,\n"+
					"or use a different instance backend.",
			),
			dialogHint.Render("[enter/q/esc] Dismiss"),
		)
		b.WriteString(warnBox.Render(dialog))
		b.WriteString("\n")

	case viewCreateSession:
		b.WriteString("\n")
		hint := fleetPage.textDialogHint("Create (empty for auto-name)")
		body := fmt.Sprintf(
			"%s\n\n%s %s\n%s %s",
			dialogTitle.Render("New session"),
			dialogLabel.Render("Instance:"),
			fleetExpandedStyle.Render(fleetPage.dialogFleet+"/"+fleetPage.dialogInst),
			dialogLabel.Render("Name:    "),
			fleetPage.textInput.View(),
		)
		// Template line, only when the fleet has layout presets to cycle. Shown
		// as a bracketed cycle option ([ none ] / [ name ]) like the backend
		// selector, with the chosen template highlighted.
		if len(fleetPage.dialogPresets) > 0 {
			var tmpl string
			if idx := fleetPage.dialogPresetIdx; idx >= 0 && idx < len(fleetPage.dialogPresets) {
				tmpl = selectedStyle.Render(fmt.Sprintf("[ %s ]", fleetPage.dialogPresets[idx].Name))
			} else {
				tmpl = dimStyle.Render("[ none ]")
			}
			body += fmt.Sprintf("\n%s %s", dialogLabel.Render("Template:"), tmpl)
			hint = "[tab] Cycle template  " + hint
		}
		dialog := fmt.Sprintf("%s\n\n%s", body, dialogHint.Render(hint))
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewCloneInstance:
		b.WriteString("\n")
		dialog := fmt.Sprintf(
			"%s\n\n%s %s\n%s %s\n\n%s",
			dialogTitle.Render("Clone instance"),
			dialogLabel.Render("Source:     "),
			fleetExpandedStyle.Render(fleetPage.dialogFleet+"/"+fleetPage.dialogInst),
			dialogLabel.Render("Destination:"),
			fleetPage.textInput.View(),
			dialogHint.Render(fleetPage.textDialogHint("Clone")),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewRenameSession:
		b.WriteString("\n")
		dialog := fmt.Sprintf(
			"%s\n\n%s %s\n%s %s\n%s %s\n\n%s",
			dialogTitle.Render("Rename session"),
			dialogLabel.Render("Instance:"),
			fleetExpandedStyle.Render(fleetPage.dialogFleet+"/"+fleetPage.dialogInst),
			dialogLabel.Render("Current: "),
			sessionStyle.Render(fleetPage.dialogSession),
			dialogLabel.Render("New:     "),
			fleetPage.textInput.View(),
			dialogHint.Render(fleetPage.textDialogHint("Rename")),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewConfirmDeleteSession:
		b.WriteString("\n")
		displayName := fleetPage.dialogSession
		if fleetPage.dialogGroupID != "" {
			displayName = fleetPage.dialogGroupID
		}
		dialog := fmt.Sprintf(
			"%s\n\n%s\n\n%s",
			dialogTitle.Render("Delete session"),
			dialogLabel.Render(fmt.Sprintf("Remove session %s from %s/%s?",
				displayName, fleetPage.dialogFleet, fleetPage.dialogInst)),
			dialogHint.Render("[y] Yes  [n/q/esc] No"),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewConfirmBrowserSwitch:
		b.WriteString("\n")
		var dialog string
		if fleetPage.dialogBrowserSwitching {
			dialog = fmt.Sprintf(
				"%s\n\n%s %s",
				dialogTitle.Render("Switch browser"),
				m.spinner.View(),
				dialogLabel.Render(fmt.Sprintf(
					"Switching browser to %s/%s...",
					fleetPage.dialogFleet, fleetPage.dialogInst,
				)),
			)
		} else {
			dialog = fmt.Sprintf(
				"%s\n\n%s\n\n%s\n\n%s",
				dialogTitle.Render("Switch browser"),
				dialogLabel.Render(fmt.Sprintf(
					"Another browser is running. Switch it to %s/%s?",
					fleetPage.dialogFleet, fleetPage.dialogInst,
				)),
				dimStyle.Render("For two at once: Settings → Browser → Multiple Browsers (separate profiles per instance)."),
				dialogHint.Render("[Y/enter] Yes (default)  [n/q/esc] No"),
			)
		}
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewArmadaSelect:
		b.WriteString("\n")
		entries := m.armadaEntries()
		var opts strings.Builder
		for i, e := range entries {
			suffix := ""
			if e.url != "" {
				suffix = "  " + armadaStatusValue(m, e.url)
			}
			if e.current {
				suffix += "  " + dimStyle.Render("(current)")
			}
			if fleetPage.armadaDialogRow == i {
				opts.WriteString(cursorStyle.Render("> ") + selectedStyle.Render(e.displayName) + suffix)
			} else {
				opts.WriteString("  " + dialogLabel.Render(e.displayName) + suffix)
			}
			opts.WriteString("\n")
		}
		dialog := fmt.Sprintf(
			"%s\n\n%s\n\n%s",
			dialogTitle.Render("Switch armada"),
			strings.TrimRight(opts.String(), "\n"),
			dialogHint.Render("[j/k] Select  [enter/space] Switch  [q/esc] Cancel"),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewChooseBrowserLaunch:
		b.WriteString("\n")
		// Cursor-selectable option line: "> " marker + pink highlight on
		// the focused row, matching the fleet list's selection style.
		optLine := func(r int, label string) string {
			if fleetPage.dialogRow == r {
				return cursorStyle.Render("> ") + selectedStyle.Render(label)
			}
			return "  " + dialogLabel.Render(label)
		}
		dialog := fmt.Sprintf(
			"%s\n\n%s\n\n%s\n%s\n\n%s\n\n%s",
			dialogTitle.Render("Choose browser start page"),
			dialogLabel.Render("This project configures both a Fleet Launch landing page and an initialUrl. Which should the browser open?"),
			optLine(chooseBrowserRowFleetLaunch, "[f] Fleet Launch"),
			optLine(chooseBrowserRowInitialURL, "[u] Initial URL"),
			dimStyle.Render("Selection will be saved to fleet settings"),
			dialogHint.Render("[j/k] Select  [enter/space] Choose  [f/u] Shortcut  [q/esc] Cancel"),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")
	}

	if m.message != "" {
		b.WriteString(messageStyle.Render(m.message))
		b.WriteString("\n")
	}

	// Focus mode hides the help bar entirely (like turning help text off) —
	// it's focus mode, after all.
	showHelp := m.config == nil || m.config.GeneralSettings.ShowHelpTextEnabled()
	if showHelp && fleetPage.focusedFleet == "" {
		b.WriteString(renderHelp(m.width, fleetPage.contextualHelpKeys(m)))
	}

	return b.String()
}

func (fleetPage *fleetPage) textDialogHint(action string) string {
	if fleetPage.dialogFieldActive {
		return fmt.Sprintf("[enter] %s  [esc] Done editing  [ctrl+c] Cancel", action)
	}
	return "[enter] Edit  [q/esc] Cancel"
}

// renderEditFleet builds the edit-fleet dialog body from the currently visible
// rows. Instant-save means there is no explicit save row — toggles persist as
// they're made and esc/q just closes.
func (fleetPage *fleetPage) renderEditFleet(m *model) string {
	marker := func(row int) string {
		if fleetPage.dialogRow == row {
			return cursorStyle.Render("> ")
		}
		return "  "
	}
	checkbox := func(on bool) string {
		if on {
			return "[x]"
		}
		return "[ ]"
	}

	var d strings.Builder
	d.WriteString(dialogTitle.Render("Edit fleet"))
	d.WriteString("\n\n")
	d.WriteString("  " + dialogLabel.Render("Fleet:    ") + " " + fleetExpandedStyle.Render(fleetPage.dialogFleet) + "\n")

	for _, row := range fleetPage.visibleEditFleetRows() {
		switch row {
		case editFleetRowClaude:
			d.WriteString(marker(row) + checkbox(fleetPage.dialogClaudeMount) + " " + dialogLabel.Render("Claude Code mount"))
		case editFleetRowCodex:
			d.WriteString(marker(row) + checkbox(fleetPage.dialogCodexMount) + " " + dialogLabel.Render("Codex mount"))
		case editFleetRowGh:
			d.WriteString(marker(row) + checkbox(fleetPage.dialogGhMount) + " " + dialogLabel.Render("GitHub CLI mount"))
		case editFleetRowAuggie:
			d.WriteString(marker(row) + checkbox(fleetPage.dialogAuggieMount) + " " + dialogLabel.Render("Auggie mount"))
		case editFleetRowHomeDir:
			// Text input when focused, dim static text otherwise; append a
			// spinner + status while an auto-detect runs.
			var field string
			if fleetPage.dialogFieldActive && fleetPage.dialogRow == editFleetRowHomeDir {
				field = fleetPage.homedirInput.View()
			} else if v := fleetPage.homedirInput.Value(); v == "" {
				field = dimStyle.Render("(unset — defaults to /home/vscode)")
			} else {
				field = v
			}
			if fleetPage.dialogDetecting {
				field += " " + m.spinner.View() + dimStyle.Render(" detecting home dir...")
			}
			d.WriteString(marker(row) + dialogLabel.Render("Home dir: ") + " " + field)
		case editFleetRowPreferFleetLaunch:
			d.WriteString(marker(row) + checkbox(fleetPage.dialogPreferFleetLaunch) + " " + dialogLabel.Render("Prefer Fleet Launch"))
		case editFleetRowLayouts:
			arrow := "▶ "
			if fleetPage.dialogLayoutsExpanded {
				arrow = "▼ "
			}
			d.WriteString(marker(row) + dialogLabel.Render(fmt.Sprintf("%sLayouts (%d)", arrow, len(fleetPage.dialogLayoutPresets))))
		case editFleetRowCustomMounts:
			arrow := "▶ "
			if fleetPage.dialogCustomMountsExpanded {
				arrow = "▼ "
			}
			d.WriteString(marker(row) + dialogLabel.Render(fmt.Sprintf("%sCustom mounts (%d)", arrow, len(fleetPage.dialogCustomMounts))))
		case editFleetRowCaching:
			arrow := "▶ "
			if fleetPage.dialogCachingExpanded {
				arrow = "▼ "
			}
			d.WriteString(marker(row) + dialogLabel.Render(fmt.Sprintf("%sCaching (%d)", arrow, fleetPage.enabledCacheCount())))
		case editFleetRowBuildkit:
			d.WriteString(marker(row) + fleetPage.renderCacheRow(m, cacheBuildkit, "Buildkit server"))
		case editFleetRowDebCache:
			d.WriteString(marker(row) + fleetPage.renderCacheRow(m, cacheDeb, "Deb package cache"))
		case editFleetRowImageCache:
			d.WriteString(marker(row) + fleetPage.renderCacheRow(m, cacheImage, "Docker image cache"))
		default:
			// Dynamic child rows, indented one level under their section
			// header: layout presets or custom mounts.
			if isLayoutPresetChildRow(row) {
				d.WriteString(fleetPage.renderLayoutPresetRow(row, marker))
			} else {
				d.WriteString(fleetPage.renderCustomMountRow(row, marker))
			}
		}
		d.WriteString("\n")
	}

	if footer := fleetPage.customMountFooter(); footer != "" {
		d.WriteString("\n  " + footer + "\n")
	}
	d.WriteString("\n  " + dimStyle.Render("Mounts apply on supported backends only") + "\n\n")
	d.WriteString(dialogHint.Render(fleetPage.editFleetHint()))
	return d.String()
}

// renderCustomMountRow renders one dynamic custom-mount child row: an existing
// mount (with a [remove] affordance) or the "+ Add mount" row (which becomes an
// inline text input while the add sub-mode is active).
func (fleetPage *fleetPage) renderCustomMountRow(row int, marker func(int) string) string {
	idx := row - editFleetRowCustomMountBase
	if idx == len(fleetPage.dialogCustomMounts) {
		// The "+ Add mount" row.
		if fleetPage.dialogAddingMount {
			return marker(row) + "  " + dialogLabel.Render("New mount: ") + fleetPage.customMountInput.View()
		}
		return marker(row) + "  " + dialogLabel.Render("+ Add mount")
	}
	return marker(row) + "  " + fleetPage.dialogCustomMounts[idx] + "   " + fleetPage.renderRemoveMountButton(row)
}

// renderLayoutPresetRow renders one dynamic layout-preset child row: an
// existing preset (name + pane count, with a [remove] affordance) or the
// "+ Layout Preset" row that starts the capture flow.
func (fleetPage *fleetPage) renderLayoutPresetRow(row int, marker func(int) string) string {
	idx := row - editFleetRowLayoutPresetBase
	if idx == len(fleetPage.dialogLayoutPresets) {
		return marker(row) + "  " + dialogLabel.Render("+ Layout Preset")
	}
	p := fleetPage.dialogLayoutPresets[idx]
	label := fmt.Sprintf("%s (%s)", p.Name, paneCountLabel(p.PaneCount()))
	return marker(row) + "  " + label + "   " + fleetPage.renderRemovePresetButton(row)
}

// renderRemovePresetButton renders the [remove] affordance next to an existing
// layout preset. Like the Caching section's [Delete cache] button it is only
// highlighted when the horizontal sub-cursor is on it (dialogPresetRemoveFocused
// on this row) — selecting the row alone leaves it dim, so it never looks armed
// before the user arrows onto it.
func (fleetPage *fleetPage) renderRemovePresetButton(row int) string {
	focused := fleetPage.dialogRow == row && fleetPage.dialogPresetRemoveFocused
	if focused && fleetPage.dialogPresetRemoveConfirm {
		return selectedStyle.Render("[remove?]")
	}
	if focused {
		return selectedStyle.Render("[remove]")
	}
	return dimStyle.Render("[remove]")
}

// renderRemoveMountButton renders the [remove] affordance next to an existing
// custom mount. It mirrors the Caching section's [Delete cache] button: dim when
// its row is not focused, highlighted when focused, and shown as a highlighted
// "[remove?]" once the inline confirm is armed.
func (fleetPage *fleetPage) renderRemoveMountButton(row int) string {
	focused := fleetPage.dialogRow == row
	if focused && fleetPage.dialogMountRemoveConfirm {
		return selectedStyle.Render("[remove?]")
	}
	if focused {
		return selectedStyle.Render("[remove]")
	}
	return dimStyle.Render("[remove]")
}

// customMountFooter returns a context line shown beneath the dialog rows while
// the cursor is on a custom-mount row: the resolved host path for an existing
// mount or the in-progress add, plus a hint or inline validation error.
func (fleetPage *fleetPage) customMountFooter() string {
	if !isCustomMountChildRow(fleetPage.dialogRow) {
		return ""
	}
	idx := fleetPage.dialogRow - editFleetRowCustomMountBase
	if idx < len(fleetPage.dialogCustomMounts) {
		return dimStyle.Render("host: " + customMountHostPreview(fleetPage.dialogFleet, fleetPage.dialogCustomMounts[idx]))
	}
	// The add row.
	if !fleetPage.dialogAddingMount {
		return ""
	}
	if fleetPage.dialogCustomMountErr != "" {
		return errorStyle.Render("✗ " + fleetPage.dialogCustomMountErr)
	}
	if v := strings.TrimSpace(fleetPage.customMountInput.Value()); v != "" {
		return dimStyle.Render("host: " + customMountHostPreview(fleetPage.dialogFleet, v))
	}
	return dimStyle.Render("enter an absolute container path, e.g. /opt/data")
}

// renderCacheRow renders one cache row (checkbox + label, indented under the
// Caching header) plus its [Delete cache] button when the cache is enabled. The
// three cache rows (buildkit/deb/image) share this rendering.
func (fleetPage *fleetPage) renderCacheRow(m *model, k cacheKind, label string) string {
	box := "[ ]"
	if fleetPage.cacheEnabled(k) {
		box = "[x]"
	}
	line := "  " + box + " " + dialogLabel.Render(label)
	if fleetPage.cacheEnabled(k) {
		line += "   " + fleetPage.renderDeleteCacheButton(m, k)
	}
	return line
}

// renderDeleteCacheButton renders the [Delete cache] button shown next to an
// enabled cache. It reflects the in-flight / inline-confirm state for cache kind
// k and is highlighted when the horizontal sub-cursor is on that row's button.
func (fleetPage *fleetPage) renderDeleteCacheButton(m *model, k cacheKind) string {
	var label string
	switch {
	case fleetPage.dialogDeleting && fleetPage.dialogDeletingKind == k:
		label = m.spinner.View() + " Clearing…"
	case fleetPage.cacheRowFocused(k) && fleetPage.dialogDeleteCacheConfirm:
		// Kept short so the row fits the 46-col dialog; the footer hint spells
		// out enter=confirm / esc=cancel.
		label = "Delete cache?"
	default:
		label = "Delete cache"
	}
	text := "[ " + label + " ]"
	if fleetPage.cacheRowFocused(k) && fleetPage.dialogCacheButtonFocused {
		return selectedStyle.Render(text)
	}
	return dimStyle.Render(text)
}

func (fleetPage *fleetPage) editFleetHint() string {
	if fleetPage.dialogFieldActive {
		return "[enter] Save  [esc] Discard edit"
	}
	if fleetPage.dialogAddingMount {
		return "[enter] Add mount  [esc] Cancel"
	}
	if isCustomMountChildRow(fleetPage.dialogRow) {
		if fleetPage.dialogRow == fleetPage.customMountAddRow() {
			return "[enter] Add mount  [j/k] Select  [q/esc] Save & Close"
		}
		if fleetPage.dialogMountRemoveConfirm {
			return "[enter] Confirm remove  [esc] Cancel"
		}
		return "[enter/d] Remove  [j/k] Select  [q/esc] Save & Close"
	}
	if isLayoutPresetChildRow(fleetPage.dialogRow) {
		if fleetPage.dialogRow == fleetPage.layoutPresetAddRow() {
			return "[enter] New preset  [j/k] Select  [q/esc] Save & Close"
		}
		if fleetPage.dialogPresetRemoveFocused {
			if fleetPage.dialogPresetRemoveConfirm {
				return "[enter] Confirm remove  [esc] Cancel"
			}
			return "[enter] Remove  [h/←] Back  [esc] Close"
		}
		return "[enter] Edit  [l/→] Remove button  [j/k] Select  [q/esc] Save & Close"
	}
	switch fleetPage.dialogRow {
	case editFleetRowCustomMounts:
		if fleetPage.dialogCustomMountsExpanded {
			return "[h/←] Collapse  [j/k] Select  [q/esc] Save & Close"
		}
		return "[l/→/space] Expand  [j/k] Select  [q/esc] Save & Close"
	case editFleetRowLayouts:
		if fleetPage.dialogLayoutsExpanded {
			return "[h/←] Collapse  [j/k] Select  [q/esc] Save & Close"
		}
		return "[l/→/space] Expand  [j/k] Select  [q/esc] Save & Close"
	case editFleetRowCaching:
		if fleetPage.dialogCachingExpanded {
			return "[h/←] Collapse  [j/k] Select  [q/esc] Save & Close"
		}
		return "[l/→/space] Expand  [j/k] Select  [q/esc] Save & Close"
	case editFleetRowBuildkit, editFleetRowDebCache, editFleetRowImageCache:
		if fleetPage.dialogCacheButtonFocused {
			if fleetPage.dialogDeleteCacheConfirm {
				return "[enter] Confirm delete  [esc] Cancel"
			}
			return "[enter] Delete cache  [h/←] Back  [esc] Close"
		}
		if k, ok := cacheKindForRow(fleetPage.dialogRow); ok && fleetPage.cacheEnabled(k) {
			return "[space] Toggle  [l/→] Delete-cache button  [j/k] Select"
		}
		return "[space] Toggle  [j/k] Select  [q/esc] Save & Close"
	case editFleetRowHomeDir:
		return "[enter] Edit  [j/k] Select  [q/esc] Save & Close"
	}
	// Flat checkbox rows: Enter/space/h/l all toggle (instant-save).
	return "[j/k] Select  [space/enter/h/l] Toggle  [q/esc] Save & Close"
}

func (fleetPage *fleetPage) portForwardHint() string {
	if fleetPage.dialogFieldActive {
		return "[enter] Add  [esc] List  [ctrl+c] Close"
	}
	return "[j/k] Navigate  [d] Delete selected  [enter] Edit add field  [q/esc] Close"
}

// ===========================================
// Session Management
// ===========================================

// openInstanceSession opens a split pane for the given instance, reusing
// the last active session when available.
func (fleetPage *fleetPage) openInstanceSession(m *model, fleetName string, instance *fleet.Instance) tea.Cmd {
	if fleetPage.restoreInProgress() {
		m.message = "Pane group restore already in progress"
		return nil
	}

	ref := InstanceRef{Fleet: fleetName, Instance: instance.Name}
	sanitized := SanitizeSessionName(instance.Name)

	// Discovery is sourced from the server runtime; hitting enter on a collapsed
	// row with no lastActive entry would otherwise always spawn a new group.
	// Populate from the runtime cache on demand so we can attach to an existing
	// session when available.
	ensureSessionsLoaded(m, ref)

	// splitSessionCmd resolves the session shell argv (server-side) and builds a
	// split-pane command, surfacing a resolve error as a status message.
	splitSessionCmd := func(sessionName, groupID string) tea.Cmd {
		cols, rows := tmuxWindowSize()
		cols = cols * 70 / 100
		shellCmd := ShellCommandForSession(m.config, sessionName, cols, rows, true)
		cmd, err := attachExecCmd(fleetName, instance.Name, shellCmd)
		if err != nil {
			m.message = fmt.Sprintf("Could not open session: %v", err)
			return nil
		}
		return splitPaneCmd(fleetPage.splitPaneID, ref, sessionName, groupID, cmd)
	}

	if last, ok := m.sessionStore.LastActive(ref); ok {
		if last.groupID != "" {
			return fleetPage.restoreGroupCmd(m, fleetName, instance, last.groupID)
		}
		return splitSessionCmd(last.sessionName, last.groupID)
	}

	if groups := m.sessionStore.Groups(ref); len(groups) > 0 {
		g := groups[0]
		rootName := g.Sessions[0].Name
		if g.GroupID != "" && isGroupedSession(sanitized, rootName) {
			return fleetPage.restoreGroupCmd(m, fleetName, instance, g.GroupID)
		}
		return splitSessionCmd(rootName, g.GroupID)
	}

	newGroupID := randomHex(3)
	sessName := GroupSessionName(sanitized, newGroupID)
	return splitSessionCmd(sessName, newGroupID)
}

// cycleSessionGroup moves the visual selection to the next or previous
// session group within the currently-split instance and starts a
// debounce timer. Group lookup is scoped to fleetPage.splitRef so two
// instances that share group IDs cannot leak into each other.
func (fleetPage *fleetPage) cycleSessionGroup(m *model, prev bool) tea.Cmd {
	if fleetPage.restoreInProgress() {
		m.message = "Pane group restore already in progress"
		return nil
	}

	groups := m.sessionStore.Groups(fleetPage.splitRef)
	if len(groups) < 2 {
		return nil
	}

	from := fleetPage.activeGroup
	if !fleetPage.pendingGroup.Empty() {
		from = fleetPage.pendingGroup
	}

	currentIdx := -1
	for i, g := range groups {
		if g.GroupID == from.GroupID && from.Ref == fleetPage.splitRef {
			currentIdx = i
			break
		}
	}
	if currentIdx < 0 {
		return nil
	}

	targetIdx := currentIdx - 1
	if !prev {
		targetIdx = currentIdx + 1
	}
	if targetIdx < 0 {
		targetIdx = len(groups) - 1
	} else if targetIdx >= len(groups) {
		targetIdx = 0
	}

	fleetPage.pendingGroup = ActiveGroup{Ref: fleetPage.splitRef, GroupID: groups[targetIdx].GroupID}
	fleetPage.debounceSeq++
	return groupCycleDebounce(fleetPage.debounceSeq)
}

// commitGroupCycle performs the actual pane switch after the debounce
// timer expires.
func (fleetPage *fleetPage) commitGroupCycle(m *model) tea.Cmd {
	if fleetPage.restoreInProgress() {
		m.message = "Pane group restore already in progress"
		return nil
	}

	if fleetPage.pendingGroup.Empty() || fleetPage.pendingGroup == fleetPage.activeGroup {
		fleetPage.pendingGroup = ActiveGroup{}
		return nil
	}

	target := fleetPage.pendingGroup
	fleetPage.pendingGroup = ActiveGroup{}

	f, ok := m.st.Fleets[target.Ref.Fleet]
	if !ok {
		return nil
	}
	instance, err := f.GetInstance(target.Ref.Instance)
	if err != nil {
		return nil
	}

	fleetPage.saveCurrentGroupLayout(m)
	killAllSplitPanes()

	fleetPage.activeGroup = target

	return fleetPage.restoreGroupCmd(m, target.Ref.Fleet, instance, target.GroupID)
}

// ===========================================
// Backend Helpers
// ===========================================

// availableBackendTypes returns the subset of backend types whose
// required CLI tool is found on the system.
func (fleetPage *fleetPage) availableBackendTypes(m *model) []fleet.BackendType {
	var out []fleet.BackendType
	for _, backendType := range allBackendTypes {
		bin := backendToolRequirements[backendType]
		if bin == "" {
			out = append(out, backendType)
			continue
		}
		for _, t := range m.toolStatus {
			if t.Binary == bin && t.Found {
				out = append(out, backendType)
				break
			}
		}
	}
	return out
}
