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

	// mode selects which dialog/interaction is active; only one is open at a
	// time, so the dialogs below reuse the shared dlg scratch and never
	// coexist. Per-dialog state is grouped into its own sub-struct (see the
	// type definitions following fleetPage) rather than flattened here.
	mode viewMode

	dlg        dialogContext      // shared scratch reused by every dialog
	addInst    addInstanceState   // add/edit-instance form
	editFleet  editFleetState     // edit-fleet settings dialog
	addFleet   addFleetState      // new-fleet URL → inspect flow
	newSession createSessionState // new-session preset templating
	browserDlg browserDialogState // switch-browser dialog
	lpFlow     *layoutPresetFlow  // open preset creation/edit flow (nil unless mode == viewLayoutPreset)

	textInput        textinput.Model
	branchInput      textinput.Model
	homedirInput     textinput.Model
	customMountInput textinput.Model

	pfCursor int

	split       splitState // open split pane + session-group restore bookkeeping
	savedGroups map[string]savedGroup

	// listRowY is the terminal Y (0-indexed) where rows[0] is rendered,
	// recorded during View() so mouse clicks can be mapped back to a
	// row index. -1 means "not yet rendered" or "no clickable rows".
	listRowY int

	armadaSel armadaSelectState // Armada selector widget (border label + dropdown)
}

// splitState holds the open split pane plus the session-group restore
// bookkeeping. ref qualifies activeGroup so two groups sharing an ID across
// instances cannot alias.
type splitState struct {
	paneID     string
	ref        InstanceRef // (fleet, instance) of the open split pane; zero when none
	session    string
	openedAt   time.Time // when the current split pane was opened; for "session closed" duration
	viaRestore bool      // true when the split was opened via restoreGroupCmd (fleet shell logs its own open/close, so the TUI must not duplicate it)

	activeGroup      ActiveGroup
	pendingGroup     ActiveGroup
	debounceSeq      int
	restoringGroupID string
	restoreSeq       int
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
		armadaSel:        armadaSelectState{y: -1},
	}
}

func (fleetPage *fleetPage) restoreInProgress() bool {
	return fleetPage.split.restoringGroupID != ""
}

func (fleetPage *fleetPage) beginGroupRestore(groupID string) int {
	fleetPage.split.restoreSeq++
	fleetPage.split.restoringGroupID = groupID
	return fleetPage.split.restoreSeq
}

func (fleetPage *fleetPage) finishGroupRestore(seq int) bool {
	if seq == 0 {
		return true
	}
	if seq != fleetPage.split.restoreSeq {
		return false
	}
	fleetPage.split.restoringGroupID = ""
	return true
}

// clearSplit resets every field that tracks the open split pane. Used
// whenever the split is closed (user toggle, external kill, restore
// teardown) so a future open starts from a known-empty state.
func (fleetPage *fleetPage) clearSplit() {
	// (The per-session open/close event log moved to the server, which owns
	// ~/.fleet/fleet.log; the split bookkeeping below is host-side only.)
	fleetPage.split.paneID = ""
	fleetPage.split.ref = InstanceRef{}
	fleetPage.split.session = ""
	fleetPage.split.openedAt = time.Time{}
	fleetPage.split.viaRestore = false
	fleetPage.split.activeGroup = ActiveGroup{}
	fleetPage.split.restoringGroupID = ""
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
		if fleetPage.browserDlg.switching {
			fleetPage.browserDlg.switching = false
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
	fleetPage.armadaSel.focused = false
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
	fleetPage.armadaSel.focused = false
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
	fleetPage.armadaSel.dialogRow = 0
	for i, e := range m.armadaEntries() {
		if e.current {
			fleetPage.armadaSel.dialogRow = i
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
		if fleetPage.armadaSel.focused {
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
				fleetPage.armadaSel.focused = false
				if i := fleetPage.lastSelectable(); i >= 0 {
					fleetPage.cursor = i
				}
			case "down", "j":
				fleetPage.armadaSel.focused = false
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
				fleetPage.armadaSel.focused = false
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
				fleetPage.armadaSel.focused = true
			} else {
				fleetPage.moveCursor(-1)
			}

		case "down", "j":
			// Down from the bottom row wraps up to the Armada selector.
			if fleetPage.cursor == fleetPage.lastSelectable() {
				fleetPage.armadaSel.focused = true
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
				fleetPage.dlg.fleet = r.fleetName
				fleetPage.dlg.inst = r.instance.Name
				fleetPage.dlg.session = r.sessionName
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
				fleetPage.dlg.fleet = r.fleetName
				fleetPage.dlg.inst = r.instance.Name
				fleetPage.dlg.session = r.sessionName
				fleetPage.dlg.groupID = r.groupID
				fleetPage.mode = viewConfirmDeleteSession
				break
			}
			fleetPage.dlg.fleet = r.fleetName
			if r.kind == rowFleetHeader {
				fleetPage.dlg.inst = ""
			} else if r.instance != nil {
				fleetPage.dlg.inst = r.instance.Name
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
			fleetPage.dlg.fleet = fleetName
			fleetPage.addInst.backend = available[0]
			if m.config != nil {
				preferred := fleet.BackendType(m.config.DefaultBackend)
				for _, backendType := range available {
					if backendType == preferred {
						fleetPage.addInst.backend = preferred
						break
					}
				}
			}
			fleetPage.addInst.color = instanceColorWhite
			fleetPage.dlg.row = addInstanceRowName
			fleetPage.addInst.editing = false
			fleetPage.dlg.fieldActive = false
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
			if m.inHostTmux && fleetPage.split.ref.Valid() && !fleetPage.split.activeGroup.Empty() {
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
			fleetPage.dlg.fleet = fleetPage.currentFleetName()
			fleetPage.dlg.inst = instance.Name
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
			fleetPage.dlg.fleet = fleetPage.currentFleetName()
			fleetPage.dlg.inst = instance.Name

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
			fleetPage.dlg.fleet = fleetPage.currentFleetName()
			fleetPage.dlg.inst = instance.Name
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
			fleetPage.dlg.fleet = fleetPage.currentFleetName()
			fleetPage.dlg.inst = instance.Name
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
			if fleetPage.split.paneID != "" && !splitOpen() {
				unbindHostSplitKeys()
				fleetPage.clearSplit()
			}
			rowGroup := ActiveGroup{Ref: sessRef, GroupID: groupID}
			// Same instance + same group → toggle split closed.
			if fleetPage.split.paneID != "" && fleetPage.split.ref == sessRef && groupID != "" && fleetPage.split.activeGroup == rowGroup {
				fleetPage.saveCurrentGroupLayout(m)
				killAllSplitPanes()
				unbindHostSplitKeys()
				fleetPage.clearSplit()
				return nil
			}
			if fleetPage.split.paneID != "" && !fleetPage.split.activeGroup.Empty() {
				fleetPage.saveCurrentGroupLayout(m)
				killAllSplitPanes()
			}
			fleetPage.split.activeGroup = rowGroup
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
			return splitPaneCmd(fleetPage.split.paneID, sessRef, sessionName, groupID, cmd)
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
			if fleetPage.split.paneID != "" && !splitOpen() {
				unbindHostSplitKeys()
				fleetPage.clearSplit()
			}
			if fleetPage.split.paneID != "" && fleetPage.split.ref == instRef {
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
		fleetPage.armadaSel.x0, fleetPage.armadaSel.x1 = -1, -1
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
	if fleetPage.armadaSel.focused || fleetPage.mode == viewArmadaSelect {
		restStyle = selectedStyle
	}
	styledLabel := " " + renderGradient("Armada") + restStyle.Render(rest)

	fleetPage.armadaSel.x0 = 3
	fleetPage.armadaSel.x1 = 3 + labelWidth
	rightDashes := max(0, width-4-labelWidth)
	return borderStyle.Render("╭──") + styledLabel + borderStyle.Render(strings.Repeat("─", rightDashes)+"╮")
}

// contextualHelpKeys returns the footer hints for the current row, adding a
// "f: focus" discovery hint on any fleet row. (Focus mode itself hides the help
// bar, so there are no in-focus hints to render.)
func (fleetPage *fleetPage) contextualHelpKeys(m *model) []string {
	keys := fleetPage.contextualHelpKeysBase(m)
	// The Armada selector swallows its own keys, so 'f' does nothing there.
	if !fleetPage.armadaSel.focused && fleetPage.currentFleetName() != "" {
		keys = insertHelpHintBefore(keys, "q: quit", "f: focus")
	}
	return keys
}

func (fleetPage *fleetPage) contextualHelpKeysBase(m *model) []string {
	if fleetPage.armadaSel.focused {
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
		if m.inHostTmux && fleetPage.split.ref.Valid() && !fleetPage.split.activeGroup.Empty() {
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
		isSelected := i == fleetPage.cursor && !fleetPage.armadaSel.focused
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
			displayGroup := fleetPage.split.activeGroup
			if !fleetPage.split.pendingGroup.Empty() {
				displayGroup = fleetPage.split.pendingGroup
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
	fleetPage.armadaSel.y = strings.Count(b.String(), "\n")
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

	b.WriteString(fleetPage.viewActiveDialog(m))

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
		return splitPaneCmd(fleetPage.split.paneID, ref, sessionName, groupID, cmd)
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

	groups := m.sessionStore.Groups(fleetPage.split.ref)
	if len(groups) < 2 {
		return nil
	}

	from := fleetPage.split.activeGroup
	if !fleetPage.split.pendingGroup.Empty() {
		from = fleetPage.split.pendingGroup
	}

	currentIdx := -1
	for i, g := range groups {
		if g.GroupID == from.GroupID && from.Ref == fleetPage.split.ref {
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

	fleetPage.split.pendingGroup = ActiveGroup{Ref: fleetPage.split.ref, GroupID: groups[targetIdx].GroupID}
	fleetPage.split.debounceSeq++
	return groupCycleDebounce(fleetPage.split.debounceSeq)
}

// commitGroupCycle performs the actual pane switch after the debounce
// timer expires.
func (fleetPage *fleetPage) commitGroupCycle(m *model) tea.Cmd {
	if fleetPage.restoreInProgress() {
		m.message = "Pane group restore already in progress"
		return nil
	}

	if fleetPage.split.pendingGroup.Empty() || fleetPage.split.pendingGroup == fleetPage.split.activeGroup {
		fleetPage.split.pendingGroup = ActiveGroup{}
		return nil
	}

	target := fleetPage.split.pendingGroup
	fleetPage.split.pendingGroup = ActiveGroup{}

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

	fleetPage.split.activeGroup = target

	return fleetPage.restoreGroupCmd(m, target.Ref.Fleet, instance, target.GroupID)
}

// ===========================================
// Backend Helpers
// ===========================================

// viewActiveDialog renders the active dialog overlay appended below the
// fleet list. It returns "" when no dialog is open (mode == viewNormal).
func (fleetPage *fleetPage) viewActiveDialog(m *model) string {
	switch fleetPage.mode {
	case viewConfirmDelete:
		return fleetPage.renderConfirmDeleteDialog(m)
	case viewConfirmRebuild:
		return fleetPage.renderConfirmRebuildDialog(m)
	case viewConfirmDeleteFleetWarn:
		return fleetPage.renderConfirmDeleteFleetWarnDialog(m)
	case viewAddInstance:
		return fleetPage.renderAddInstanceDialog(m)
	case viewAddFleet:
		return fleetPage.renderAddFleetDialog(m)
	case viewAddFleetInspecting:
		return fleetPage.renderAddFleetInspectingDialog(m)
	case viewAddFleetNoDevcontainer:
		return fleetPage.renderAddFleetNoDevcontainerDialog(m)
	case viewEditFleet:
		return fleetPage.renderEditFleetDialog(m)
	case viewLayoutPreset:
		return fleetPage.renderLayoutPresetOverlay(m)
	case viewTagInstance:
		return fleetPage.renderTagInstanceDialog(m)
	case viewPortForward:
		return fleetPage.renderPortForwardDialog(m)
	case viewCodespacesAuth:
		return fleetPage.renderCodespacesAuthDialog(m)
	case viewCodespacesMachine:
		return fleetPage.renderCodespacesMachineDialog(m)
	case viewCodespacesLimit:
		return fleetPage.renderCodespacesLimitDialog(m)
	case viewCreateSession:
		return fleetPage.renderCreateSessionDialog(m)
	case viewCloneInstance:
		return fleetPage.renderCloneInstanceDialog(m)
	case viewRenameSession:
		return fleetPage.renderRenameSessionDialog(m)
	case viewConfirmDeleteSession:
		return fleetPage.renderConfirmDeleteSessionDialog(m)
	case viewConfirmBrowserSwitch:
		return fleetPage.renderConfirmBrowserSwitchDialog(m)
	case viewArmadaSelect:
		return fleetPage.renderArmadaSelectDialog(m)
	case viewChooseBrowserLaunch:
		return fleetPage.renderChooseBrowserLaunchDialog(m)
	}
	return ""
}

func (fleetPage *fleetPage) renderLayoutPresetOverlay(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(dialogBox.Render(fleetPage.renderLayoutPresetDialog()))
	b.WriteString("\n")

	return b.String()
}
