package tui

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/gitutil"
	"github.com/BenjaminBenetti/fleet-man/internal/instanceops"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// ===========================================
// Fleet Page
// ===========================================

var toggleInstanceStatus = instanceops.ToggleInstance
var resolveWorkspaceBranch = gitutil.BranchName

// fleetPage holds fleet-list-specific state.
type fleetPage struct {
	rows      []row
	cursor    int
	collapsed map[string]bool

	mode          viewMode
	dialogFleet   string
	dialogInst    string
	dialogBackend fleet.BackendType
	dialogColor   string
	dialogRow     int
	dialogEditing bool
	dialogGroupID string
	dialogSession string
	textInput     textinput.Model
	branchInput   textinput.Model

	pfCursor      int
	pfContainerID string

	splitPaneID   string
	splitFleet    string
	splitInstance string
	splitSession  string

	activeGroupID  string
	savedGroups    map[string]savedGroup
	pendingGroupID string
	debounceSeq    int

	// listRowY is the terminal Y (0-indexed) where rows[0] is rendered,
	// recorded during View() so mouse clicks can be mapped back to a
	// row index. -1 means "not yet rendered" or "no clickable rows".
	listRowY int
}

// newFleetPage creates a new fleet page with default state.
func newFleetPage() *fleetPage {
	ti := textinput.New()
	ti.Placeholder = "instance-name"
	ti.CharLimit = 64

	bi := textinput.New()
	bi.Placeholder = "default branch"
	bi.CharLimit = 128

	return &fleetPage{
		collapsed:   make(map[string]bool),
		savedGroups: make(map[string]savedGroup),
		textInput:   ti,
		branchInput: bi,
		listRowY:    -1,
	}
}

// Init is called when the fleet page becomes active.
func (fleetPage *fleetPage) Init(m *model) tea.Cmd {
	fleetPage.buildRows(m)
	return nil
}

// Update dispatches messages to the appropriate handler based on the
// current view mode.
func (fleetPage *fleetPage) Update(m *model, msg tea.Msg) tea.Cmd {
	// Fleet-specific async messages that need row rebuilds
	switch msg.(type) {
	case sessionDiscoveryMsg:
		fleetPage.buildRows(m)
		// While a split is open, re-snapshot the active group's tmux
		// layout into savedGroups on every tick. This keeps the map
		// honest against mutations that bypass fleet's handlers —
		// %/" adds new panes via a tmux binding, mouse drag resizes,
		// Ctrl+Q/O closes without notifying fleet. The save is
		// diff-gated so idle ticks don't hit disk.
		if fleetPage.splitPaneID != "" && fleetPage.activeGroupID != "" && splitOpen() {
			fleetPage.saveCurrentGroupLayout(m.st)
		}
		// Detect when panes were killed externally. savedGroups already
		// holds the pre-kill snapshot from the tick before, so clearing
		// transient fields here loses nothing.
		if fleetPage.splitPaneID != "" && !splitOpen() {
			unbindHostSplitKeys()
			fleetPage.splitPaneID = ""
			fleetPage.splitFleet = ""
			fleetPage.splitInstance = ""
			fleetPage.splitSession = ""
			fleetPage.activeGroupID = ""
		}
		return nil

	case operationDoneMsg:
		fleetPage.buildRows(m)
		return nil

	case instanceCreateErrMsg:
		fleetPage.buildRows(m)
		return nil

	case sessionsMsg:
		fleetPage.buildRows(m)
		return nil

	case browserProxyMsg:
		bpm := msg.(browserProxyMsg)
		if bpm.err != nil {
			m.message = fmt.Sprintf("Browser proxy error: %v", bpm.err)
		} else {
			m.message = fmt.Sprintf("Browser opened (proxy on localhost:%d)", bpm.localPort)
		}
		return nil
	}

	// Mode-specific dispatch
	switch fleetPage.mode {
	case viewConfirmDelete:
		return fleetPage.updateConfirmDelete(m, msg)
	case viewConfirmDeleteFleetWarn:
		return fleetPage.updateConfirmDeleteFleetWarn(m, msg)
	case viewAddInstance:
		return fleetPage.updateAddInstance(m, msg)
	case viewAddFleet:
		return fleetPage.updateAddFleet(m, msg)
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
	case viewCreateSession:
		return fleetPage.updateCreateSession(m, msg)
	case viewRenameSession:
		return fleetPage.updateRenameSession(m, msg)
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
	wasOnSettings := false
	if r := fleetPage.currentRow(); r != nil && r.kind == rowSettings {
		wasOnSettings = true
	}

	fleetPage.rows = nil

	names := sortedFleetNames(m.st.Fleets)

	for _, name := range names {
		f := m.st.Fleets[name]
		fleetPage.rows = append(fleetPage.rows, row{kind: rowFleetHeader, fleetName: name})
		if !fleetPage.collapsed[name] {
			for _, instance := range f.Instances {
				fleetPage.rows = append(fleetPage.rows, row{kind: rowInstance, fleetName: name, instance: instance})
				instKey := name + "/" + instance.Name
				if m.expandedInstances[instKey] {
					if disc, ok := m.sessions[instKey]; ok && disc.err == nil {
						sanitized := SanitizeSessionName(instance.Name)
						groups := groupSessions(sanitized, disc.sessions)
						for _, g := range groups {
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
					}
					fleetPage.rows = append(fleetPage.rows, row{
						kind:      rowNewSession,
						fleetName: name,
						instance:  instance,
					})
				}
			}
		}
	}
	fleetPage.rows = append(fleetPage.rows, row{kind: rowSettings})
	if wasOnSettings {
		fleetPage.cursor = len(fleetPage.rows) - 1
	}
	if fleetPage.cursor >= len(fleetPage.rows) {
		fleetPage.cursor = max(0, len(fleetPage.rows)-1)
	}
}

// currentRow returns a pointer to the row at the cursor position.
func (fleetPage *fleetPage) currentRow() *row {
	if fleetPage.cursor < 0 || fleetPage.cursor >= len(fleetPage.rows) {
		return nil
	}
	return &fleetPage.rows[fleetPage.cursor]
}

// moveCursor moves the cursor by delta, wrapping around.
func (fleetPage *fleetPage) moveCursor(delta int) {
	if len(fleetPage.rows) == 0 || delta == 0 {
		return
	}
	fleetPage.cursor = (fleetPage.cursor + delta + len(fleetPage.rows)) % len(fleetPage.rows)
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
	if r == nil || r.kind == rowSettings {
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

		switch msg.String() {
		case "q", "ctrl+c", "ctrl+q":
			m.quitting = true
			return tea.Quit

		case "up", "k":
			fleetPage.moveCursor(-1)

		case "down", "j":
			fleetPage.moveCursor(1)

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
					instKey := r.fleetName + "/" + r.instance.Name
					if m.expandedInstances[instKey] {
						delete(m.expandedInstances, instKey)
						fleetPage.buildRows(m)
					} else {
						m.expandedInstances[instKey] = true
						fleetPage.buildRows(m)
						b := m.instanceBackend(r.instance)
						return listSessionsCmd(b, r.instance.WorkspaceDir, instKey)
					}
				case rowSession, rowNewSession, rowSettings:
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
				fleetPage.textInput.Focus()
				return fleetPage.textInput.Cursor.BlinkCmd()
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

			if r.instance.Status == fleet.StatusRunning {
				r.instance.Status = fleet.StatusStopping
			} else if r.instance.Status == fleet.StatusStopped {
				r.instance.Status = fleet.StatusStarting
			}
			_ = state.Save(m.st)
			fleetPage.buildRows(m)

			fleetName, instName := r.fleetName, r.instance.Name
			return toggleInstanceCmd(fleetName, instName)

		case "d":
			r := fleetPage.currentRow()
			if r == nil || r.kind == rowSettings || r.kind == rowNewSession {
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
				fleetPage.mode = viewCreateSession
				fleetPage.dialogFleet = r.fleetName
				fleetPage.dialogInst = instance.Name
				fleetPage.textInput.SetValue("")
				fleetPage.textInput.Placeholder = "session-name (or empty for auto)"
				fleetPage.textInput.CharLimit = 64
				fleetPage.textInput.Focus()
				return fleetPage.textInput.Cursor.BlinkCmd()
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
			fleetPage.textInput.SetValue("")
			fleetPage.textInput.Placeholder = "instance-name"
			fleetPage.textInput.CharLimit = 64
			fleetPage.textInput.Focus()
			fleetPage.branchInput.SetValue("")
			fleetPage.branchInput.Placeholder = "default branch"
			fleetPage.branchInput.CharLimit = 128
			fleetPage.branchInput.Blur()
			return fleetPage.textInput.Cursor.BlinkCmd()

		case "n":
			fleetPage.mode = viewAddFleet
			fleetPage.textInput.SetValue("")
			fleetPage.textInput.Placeholder = "git@github.com:org/repo.git"
			fleetPage.textInput.CharLimit = 256
			fleetPage.textInput.Focus()
			return fleetPage.textInput.Cursor.BlinkCmd()

		case "pgup", "pgdown":
			if m.inHostTmux && fleetPage.splitInstance != "" && fleetPage.activeGroupID != "" {
				return fleetPage.cycleSessionGroup(m, msg.String() == "pgup")
			}

		case "enter":
			return fleetPage.handleEnter(m)

		case "e":
			return fleetPage.openEditInstanceDialog(m)

		case "o":
			_, instance := fleetPage.selectedInstance(m)
			if instance == nil {
				m.message = "Select an instance"
				break
			}
			cmd := m.instanceBackend(instance).ExecCommand(instance.WorkspaceDir, freshShellCommand(m.config))
			err := openInTerminal(cmd.Args)
			if err != nil {
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
			switch instance.Backend {
			case fleet.BackendCoder:
				codeCmd = exec.Command("coder", backendutil.CoderOpenVSCodeArgs(instance.ContainerID)...)
			case fleet.BackendCodespaces:
				codeCmd = exec.Command("gh", "codespace", "code", "-c", instance.ContainerID)
			default:
				uri, ok := m.instanceBackend(instance).EditorURI(instance.WorkspaceDir, r.fleetName)
				if !ok {
					m.message = "Editor integration not supported by this backend"
					break
				}
				codeCmd = exec.Command("code", "--folder-uri", uri)
			}
			if codeCmd != nil {
				if err := codeCmd.Run(); err != nil {
					m.message = fmt.Sprintf("VS Code error: %v", err)
				} else {
					m.message = fmt.Sprintf("Opened VS Code for %s", instance.GetDisplayName())
				}
			}

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
			b := m.instanceBackend(instance)
			instanceKey := fleetPage.currentFleetName() + "/" + instance.Name
			m.message = fmt.Sprintf("Starting browser proxy for %s...", instance.GetDisplayName())
			return openBrowserProxyCmd(m.portForwards, b, instance, instanceKey)

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
			fleetPage.textInput.Focus()
			return fleetPage.textInput.Cursor.BlinkCmd()

		case "l":
			_, instance := fleetPage.selectedInstance(m)
			if instance == nil {
				m.message = "Select an instance"
				break
			}
			r := fleetPage.rows[fleetPage.cursor]
			return tea.ExecProcess(
				logsCommand(m.instanceBackend(instance), r.fleetName, instance),
				func(err error) tea.Msg { return execDoneMsg{err} },
			)

		case "f":
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
			fleetPage.pfContainerID = instance.ContainerID
			fleetPage.pfCursor = 0
			fleetPage.textInput.SetValue("")
			fleetPage.textInput.Placeholder = "local:remote (e.g. 8080:80)"
			fleetPage.textInput.CharLimit = 11
			fleetPage.textInput.Focus()
			return fleetPage.textInput.Cursor.BlinkCmd()
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

	case rowFleetHeader:
		name := r.fleetName
		fleetPage.collapsed[name] = !fleetPage.collapsed[name]
		fleetPage.buildRows(m)

	case rowNewSession:
		instance := r.instance
		fleetPage.mode = viewCreateSession
		fleetPage.dialogFleet = r.fleetName
		fleetPage.dialogInst = instance.Name
		fleetPage.textInput.SetValue("")
		fleetPage.textInput.Placeholder = "session-name (or empty for auto)"
		fleetPage.textInput.CharLimit = 64
		fleetPage.textInput.Focus()
		return fleetPage.textInput.Cursor.BlinkCmd()

	case rowSession:
		instance := r.instance
		sessionName := r.sessionName
		groupID := r.groupID
		sessInstKey := r.fleetName + "/" + instance.Name
		m.lastActive[sessInstKey] = lastSession{sessionName: sessionName, groupID: groupID}
		if m.inHostTmux {
			if fleetPage.splitPaneID != "" && !splitOpen() {
				unbindHostSplitKeys()
				fleetPage.splitPaneID = ""
				fleetPage.splitFleet = ""
				fleetPage.splitInstance = ""
				fleetPage.splitSession = ""
				fleetPage.activeGroupID = ""
			}
			if fleetPage.splitPaneID != "" && fleetPage.splitInstance == instance.Name && groupID != "" && groupID == fleetPage.activeGroupID {
				fleetPage.saveCurrentGroupLayout(m.st)
				killAllSplitPanes()
				unbindHostSplitKeys()
				fleetPage.splitPaneID = ""
				fleetPage.splitFleet = ""
				fleetPage.splitInstance = ""
				fleetPage.splitSession = ""
				fleetPage.activeGroupID = ""
				return nil
			}
			if fleetPage.splitPaneID != "" && fleetPage.activeGroupID != "" {
				fleetPage.saveCurrentGroupLayout(m.st)
				killAllSplitPanes()
			}
			fleetPage.activeGroupID = groupID
			if groupID != "" && isGroupedSession(SanitizeSessionName(instance.Name), sessionName) {
				return fleetPage.restoreGroupCmd(m, r.fleetName, instance, groupID)
			}
			cols, rows := tmuxWindowSize()
			cols = cols * 70 / 100
			cmd := m.instanceBackend(instance).ExecCommand(
				instance.WorkspaceDir,
				ShellCommandForSession(m.config, sessionName, cols, rows, true),
			)
			return splitPaneCmd(fleetPage.splitPaneID, r.fleetName, instance.Name, sessionName, groupID, cmd)
		}
		cmd := m.instanceBackend(instance).ExecCommand(
			instance.WorkspaceDir,
			ShellCommandForSession(m.config, sessionName, m.width, m.height, false),
		)
		banner := renderGradient(nameToBanner(instance.GetDisplayName()))
		banner += "\n  " + dimStyle.Render("ctrl+q/ctrl+o to detach (session persists)")
		return tea.ExecProcess(
			execWithBannerCmd(banner, cmd),
			func(err error) tea.Msg { return execDoneMsg{err} },
		)

	case rowInstance:
		_, instance := fleetPage.selectedInstance(m)
		if instance == nil {
			break
		}
		instFleetName := r.fleetName
		instKey := instFleetName + "/" + instance.Name
		if m.inHostTmux {
			if fleetPage.splitPaneID != "" && !splitOpen() {
				unbindHostSplitKeys()
				fleetPage.splitPaneID = ""
				fleetPage.splitFleet = ""
				fleetPage.splitInstance = ""
				fleetPage.splitSession = ""
				fleetPage.activeGroupID = ""
			}
			if fleetPage.splitPaneID != "" && fleetPage.splitInstance == instance.Name {
				fleetPage.saveCurrentGroupLayout(m.st)
				killAllSplitPanes()
				unbindHostSplitKeys()
				fleetPage.splitPaneID = ""
				fleetPage.splitFleet = ""
				fleetPage.splitInstance = ""
				fleetPage.splitSession = ""
				fleetPage.activeGroupID = ""
				return nil
			}
			return fleetPage.openInstanceSession(m, instFleetName, instance)
		}

		sessionName := SanitizeSessionName(instance.Name)
		if last, ok := m.lastActive[instKey]; ok {
			sessionName = last.sessionName
		}
		m.lastActive[instKey] = lastSession{sessionName: sessionName}

		cmd := m.instanceBackend(instance).ExecCommand(instance.WorkspaceDir, ShellCommandForSession(m.config, sessionName, m.width, m.height, false))
		banner := renderGradient(nameToBanner(instance.GetDisplayName()))
		banner += "\n  " + dimStyle.Render("ctrl+q/ctrl+o to detach (session persists)")
		return tea.ExecProcess(
			execWithBannerCmd(banner, cmd),
			func(err error) tea.Msg { return execDoneMsg{err} },
		)
	}

	return nil
}

// ===========================================
// Help Keys
// ===========================================

// contextualHelpKeys returns the help bar entries relevant to the
// currently selected row and its state.
func (fleetPage *fleetPage) contextualHelpKeys(m *model) []string {
	r := fleetPage.currentRow()
	if r == nil {
		return []string{"n: new fleet", "q: quit"}
	}

	switch r.kind {
	case rowFleetHeader:
		return []string{
			"j/k: navigate", "space: expand/collapse", "enter/e: toggle",
			"a: add instance", "n: new fleet", "d: delete fleet", "r: refresh", "q: quit",
		}

	case rowInstance:
		keys := []string{"j/k: navigate"}
		if r.instance != nil {
			switch {
			case r.instance.Status == fleet.StatusRunning:
				keys = append(keys,
					"space: show sessions", "enter/e: open shell",
					"s: stop", "a: new session", "d: delete", "t: tag",
					"f: port-forward", "b: browser", "c: code", "o: terminal", "l: logs",
					"r: refresh", "q: quit",
				)
			case r.instance.Status == fleet.StatusStopped:
				keys = append(keys,
					"enter/e: open shell", "s: start",
					"a: new session", "d: delete", "t: tag", "r: refresh", "q: quit",
				)
			case r.instance.Status == fleet.StatusFailed:
				keys = append(keys, "d: delete", "r: refresh", "q: quit")
			case isTransitional(r.instance.Status):
				keys = append(keys, "r: refresh", "q: quit")
			default:
				keys = append(keys, "r: refresh", "q: quit")
			}
		}
		return keys

	case rowSession:
		keys := []string{
			"j/k: navigate", "space/enter/e: connect",
			"a: new session", "d: delete session", "r: rename", "q: quit",
		}
		if m.inHostTmux && fleetPage.splitInstance != "" && fleetPage.activeGroupID != "" {
			keys = append(keys[:len(keys)-1], "pgup/pgdn: cycle groups", "q: quit")
		}
		return keys

	case rowNewSession:
		return []string{
			"j/k: navigate", "space/enter/e: create session",
			"a: new session", "q: quit",
		}

	case rowSettings:
		return []string{
			"j/k: navigate", "space/enter/e: open settings",
			"n: new fleet", "q: quit",
		}
	}

	return []string{"q: quit"}
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
	b.WriteString(renderGradient(logo))
	if version.Version != "" {
		b.WriteString(" " + dimStyle.Render(version.Version))
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
		isSelected := i == fleetPage.cursor
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
			displayGroupID := fleetPage.activeGroupID
			if fleetPage.pendingGroupID != "" {
				displayGroupID = fleetPage.pendingGroupID
			}
			if r.groupID != "" && r.groupID == displayGroupID {
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
				listContent.WriteString(fmt.Sprintf("%s      %s", cursor, selectedStyle.Render(label)))
			} else {
				listContent.WriteString(fmt.Sprintf("%s      %s", cursor, style.Render(label)))
			}
			listContent.WriteString("\n")

		} else if r.kind == rowNewSession {
			label := "+ new session"
			if isSelected {
				listContent.WriteString(fmt.Sprintf("%s      %s", cursor, selectedStyle.Render(label)))
			} else {
				listContent.WriteString(fmt.Sprintf("%s      %s", cursor, newSessionStyle.Render(label)))
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

			instKey := r.fleetName + "/" + instance.Name
			arrow := "  "
			if instance.Status == fleet.StatusRunning {
				if m.expandedInstances[instKey] {
					arrow = "▼ "
				} else {
					arrow = "▶ "
				}
			}

			// Single switch derives BOTH the left-of-name throbber
			// and the right-of-status agentStr. Throbber animates
			// only in the working ("play") state; every other case
			// (waiting, not running, instance not running) leaves
			// the throbber at the default grey ○. agentStr is
			// consumed below in the non-transitional render branch.
			throbber := agentOffStyle.Render("○")
			agentStr := ""
			if instance.Status == fleet.StatusRunning && m.activity != nil {
				tool := m.activity.Tool(instance.ContainerID)
				label := agentToolLabel(tool)
				switch m.activity.State(instance.ContainerID) {
				case agentWorking:
					agentStr = agentWorkingStyle.Render(fmt.Sprintf("  ▶ %s", label))
					if len(m.agentSpinner.Spinner.Frames) > 0 {
						throbber = strings.TrimRight(m.agentSpinner.View(), "\n")
					} else {
						throbber = agentWorkingStyle.Render("✻")
					}
				case agentWaiting:
					agentStr = agentWaitingStyle.Render(fmt.Sprintf("  ⏸ %s", label))
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
				if s, ok := m.stats[instance.ContainerID]; ok {
					statsStr = dimStyle.Render(fmt.Sprintf("  %4.0f mcpu  %6.1f MB", s.CPUMillicores, s.MemoryMB))
				}

				line = fmt.Sprintf("%s    %s %s%s%s%s",
					cursor, paddedName, status, agentStr, statsStr, branchItem,
				)

				pfKey := r.fleetName + "/" + instance.Name
				if pfLabel := m.portForwards.FormatLabels(pfKey); pfLabel != "" {
					line += portForwardStyle.Render("  ⇄ " + pfLabel)
				}

				if instance.Tag != "" {
					line += dimStyle.Render("  # " + instance.Tag)
				}
			}

			if maxW := m.width - 4; maxW > 0 && lipgloss.Width(line) > maxW {
				line = ansi.Truncate(line, maxW-1, "…")
			}

			listContent.WriteString(line)
			listContent.WriteString("\n")
		} else {
			label := "settings"
			if isSelected {
				listContent.WriteString(fmt.Sprintf("%s%s", cursor, selectedStyle.Render(label)))
			} else {
				listContent.WriteString(fmt.Sprintf("%s%s", cursor, dimStyle.Render(label)))
			}
			listContent.WriteString("\n")
		}
	}

	boxContent := strings.TrimRight(listContent.String(), "\n")
	box := listBox
	if m.width > 0 {
		box = box.Width(m.width - 2)
	}
	// Record where rows[0] will land on screen so mouse clicks can map
	// Y → row index. The cursor is at line `newlines` after consuming
	// `b` so far; +1 skips the box top border, +emptyMsgLines skips the
	// "No instances" line that precedes the (settings-only) rows when
	// no fleets exist.
	emptyMsgLines := 0
	if m.st == nil || len(m.st.Fleets) == 0 {
		emptyMsgLines = 1
	}
	fleetPage.listRowY = strings.Count(b.String(), "\n") + 1 + emptyMsgLines
	b.WriteString(box.Render(boxContent))
	b.WriteString("\n")

	var totalCPU float64
	var totalMem float64
	for _, s := range m.stats {
		totalCPU += s.CPUMillicores
		totalMem += s.MemoryMB
	}
	if len(m.stats) > 0 {
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
			dialogHint.Render("[y] Yes  [n] No"),
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
			dialogHint.Render("[y] Confirm destroy  [n] Cancel"),
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
			hint = "[↑↓] Select  [←→/space] Cycle color  [shift+tab] Color  [enter] Save  [esc] Cancel"
			nameField = fleetPage.textInput.View()
			branchDisplay := fleetPage.branchInput.Value()
			if branchDisplay == "" {
				branchDisplay = "default"
			}
			branchField = dimStyle.Render(branchDisplay)
			deployField = dimStyle.Render(fmt.Sprintf("[ %s ]", backendTypeLabel(backendType)))
		} else {
			title = "New instance"
			hint = "[↑↓] Select  [←→/space] Cycle  [shift+tab] Color  [enter] Create  [esc] Cancel"
			if len(fleetPage.availableBackendTypes(m)) > 1 {
				hint = "[↑↓] Select  [←→/space/tab] Cycle  [shift+tab] Color  [enter] Create  [esc] Cancel"
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
			dialogHint.Render("[enter] Add  [esc] Cancel"),
		)
		b.WriteString(dialogBox.Render(dialog))
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
			dialogHint.Render("[enter] Save  [esc] Cancel"),
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
			dialogHint.Render("[enter] Add  [d] Delete selected  [j/k] Navigate  [esc] Close"),
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
			dialogHint.Render("[enter] Authenticate  [esc] Cancel"),
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
			dialogHint.Render("[enter] Open Settings  [esc] Cancel"),
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
			dialogHint.Render("[enter/esc] Dismiss"),
		)
		b.WriteString(warnBox.Render(dialog))
		b.WriteString("\n")

	case viewCreateSession:
		b.WriteString("\n")
		dialog := fmt.Sprintf(
			"%s\n\n%s %s\n%s %s\n\n%s",
			dialogTitle.Render("New session"),
			dialogLabel.Render("Instance:"),
			fleetExpandedStyle.Render(fleetPage.dialogFleet+"/"+fleetPage.dialogInst),
			dialogLabel.Render("Name:    "),
			fleetPage.textInput.View(),
			dialogHint.Render("[enter] Create (empty for auto-name)  [esc] Cancel"),
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
			dialogHint.Render("[enter] Rename  [esc] Cancel"),
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
			dialogHint.Render("[y] Yes  [n] No"),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")
	}

	if m.message != "" {
		b.WriteString(messageStyle.Render(m.message))
		b.WriteString("\n")
	}

	if m.config == nil || m.config.GeneralSettings.ShowHelpTextEnabled() {
		b.WriteString(renderHelp(m.width, fleetPage.contextualHelpKeys(m)))
	}

	return b.String()
}

// ===========================================
// Session Management
// ===========================================

// openInstanceSession opens a split pane for the given instance, reusing
// the last active session when available.
func (fleetPage *fleetPage) openInstanceSession(m *model, fleetName string, instance *fleet.Instance) tea.Cmd {
	instKey := fleetName + "/" + instance.Name
	sanitized := SanitizeSessionName(instance.Name)

	// The session discovery loop only runs for expanded instances, so
	// hitting enter on a collapsed row with no lastActive entry would
	// otherwise always spawn a new group. Load sessions on demand here
	// so we can attach to an existing one when available.
	ensureSessionsLoaded(m, m.instanceBackend(instance), instance.WorkspaceDir, instKey)

	if last, ok := m.lastActive[instKey]; ok {
		if last.groupID != "" {
			return fleetPage.restoreGroupCmd(m, fleetName, instance, last.groupID)
		}
		cols, rows := tmuxWindowSize()
		cols = cols * 70 / 100
		cmd := m.instanceBackend(instance).ExecCommand(
			instance.WorkspaceDir,
			ShellCommandForSession(m.config, last.sessionName, cols, rows, true),
		)
		return splitPaneCmd(fleetPage.splitPaneID, fleetName, instance.Name, last.sessionName, last.groupID, cmd)
	}

	if disc, ok := m.sessions[instKey]; ok && disc.err == nil && len(disc.sessions) > 0 {
		groups := groupSessions(sanitized, disc.sessions)
		if len(groups) > 0 {
			g := groups[0]
			rootName := g.Sessions[0].Name
			if g.GroupID != "" && isGroupedSession(sanitized, rootName) {
				return fleetPage.restoreGroupCmd(m, fleetName, instance, g.GroupID)
			}
			cols, rows := tmuxWindowSize()
			cols = cols * 70 / 100
			cmd := m.instanceBackend(instance).ExecCommand(
				instance.WorkspaceDir,
				ShellCommandForSession(m.config, rootName, cols, rows, true),
			)
			return splitPaneCmd(fleetPage.splitPaneID, fleetName, instance.Name, rootName, g.GroupID, cmd)
		}
	}

	newGroupID := randomHex(3)
	sessName := groupSessionName(sanitized, newGroupID)
	cols, rows := tmuxWindowSize()
	cols = cols * 70 / 100
	cmd := m.instanceBackend(instance).ExecCommand(
		instance.WorkspaceDir,
		ShellCommandForSession(m.config, sessName, cols, rows, true),
	)
	return splitPaneCmd(fleetPage.splitPaneID, fleetName, instance.Name, sessName, newGroupID, cmd)
}

// instanceGroups returns the session groups for the given instance name.
func (fleetPage *fleetPage) instanceGroups(m *model, instanceName string) []sessionGroup {
	sanitized := SanitizeSessionName(instanceName)
	for _, disc := range m.sessions {
		if disc == nil || disc.err != nil {
			continue
		}
		g := groupSessions(sanitized, disc.sessions)
		if len(g) > 0 {
			return g
		}
	}
	return nil
}

// cycleSessionGroup moves the visual selection to the next or previous
// session group and starts a debounce timer.
func (fleetPage *fleetPage) cycleSessionGroup(m *model, prev bool) tea.Cmd {
	groups := fleetPage.instanceGroups(m, fleetPage.splitInstance)
	if len(groups) < 2 {
		return nil
	}

	fromID := fleetPage.activeGroupID
	if fleetPage.pendingGroupID != "" {
		fromID = fleetPage.pendingGroupID
	}

	currentIdx := -1
	for i, g := range groups {
		if g.GroupID == fromID {
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

	fleetPage.pendingGroupID = groups[targetIdx].GroupID
	fleetPage.debounceSeq++
	return groupCycleDebounce(fleetPage.debounceSeq)
}

// commitGroupCycle performs the actual pane switch after the debounce
// timer expires.
func (fleetPage *fleetPage) commitGroupCycle(m *model) tea.Cmd {
	if fleetPage.pendingGroupID == "" || fleetPage.pendingGroupID == fleetPage.activeGroupID {
		fleetPage.pendingGroupID = ""
		return nil
	}

	var instance *fleet.Instance
	for _, f := range m.st.Fleets {
		for _, i := range f.Instances {
			if i.Name == fleetPage.splitInstance {
				instance = i
				break
			}
		}
		if instance != nil {
			break
		}
	}
	if instance == nil {
		fleetPage.pendingGroupID = ""
		return nil
	}

	targetGroupID := fleetPage.pendingGroupID
	fleetPage.pendingGroupID = ""

	fleetPage.saveCurrentGroupLayout(m.st)
	killAllSplitPanes()

	fleetPage.activeGroupID = targetGroupID

	return fleetPage.restoreGroupCmd(m, fleetPage.splitFleet, instance, targetGroupID)
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
