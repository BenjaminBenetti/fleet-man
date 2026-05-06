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
// View Mode
// ===========================================

type viewMode int

const (
	viewNormal viewMode = iota
	viewConfirmDelete
	viewConfirmDeleteFleetWarn
	viewAddInstance
	viewAddFleet
	viewTagInstance
	viewPortForward
	viewCodespacesAuth
	viewCodespacesLimit
	viewCodespacesMachine
	viewCreateSession
	viewRenameSession
	viewConfirmDeleteSession
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
	}
}

// Init is called when the fleet page becomes active.
func (fp *fleetPage) Init(m *model) tea.Cmd {
	fp.buildRows(m)
	return nil
}

// Update dispatches messages to the appropriate handler based on the
// current view mode.
func (fp *fleetPage) Update(m *model, msg tea.Msg) tea.Cmd {
	// Fleet-specific async messages that need row rebuilds
	switch msg.(type) {
	case sessionDiscoveryMsg:
		fp.buildRows(m)
		// While a split is open, re-snapshot the active group's tmux
		// layout into savedGroups on every tick. This keeps the map
		// honest against mutations that bypass fleet's handlers —
		// %/" adds new panes via a tmux binding, mouse drag resizes,
		// Ctrl+Q/O closes without notifying fleet. The save is
		// diff-gated so idle ticks don't hit disk.
		if fp.splitPaneID != "" && fp.activeGroupID != "" && splitOpen() {
			fp.saveCurrentGroupLayout(m.st)
		}
		// Detect when panes were killed externally. savedGroups already
		// holds the pre-kill snapshot from the tick before, so clearing
		// transient fields here loses nothing.
		if fp.splitPaneID != "" && !splitOpen() {
			unbindHostSplitKeys()
			fp.splitPaneID = ""
			fp.splitFleet = ""
			fp.splitInstance = ""
			fp.splitSession = ""
			fp.activeGroupID = ""
		}
		return nil

	case operationDoneMsg:
		fp.buildRows(m)
		return nil

	case instanceCreateErrMsg:
		fp.buildRows(m)
		return nil

	case sessionsMsg:
		fp.buildRows(m)
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
	switch fp.mode {
	case viewConfirmDelete:
		return fp.updateConfirmDelete(m, msg)
	case viewConfirmDeleteFleetWarn:
		return fp.updateConfirmDeleteFleetWarn(m, msg)
	case viewAddInstance:
		return fp.updateAddInstance(m, msg)
	case viewAddFleet:
		return fp.updateAddFleet(m, msg)
	case viewTagInstance:
		return fp.updateTagInstance(m, msg)
	case viewPortForward:
		return fp.updatePortForward(m, msg)
	case viewCodespacesAuth:
		return fp.updateCodespacesAuth(m, msg)
	case viewCodespacesLimit:
		return fp.updateCodespacesLimit(m, msg)
	case viewCodespacesMachine:
		return fp.updateCodespacesMachine(m, msg)
	case viewConfirmDeleteSession:
		return fp.updateConfirmDeleteSession(m, msg)
	case viewCreateSession:
		return fp.updateCreateSession(m, msg)
	case viewRenameSession:
		return fp.updateRenameSession(m, msg)
	default:
		return fp.updateNormal(m, msg)
	}
}

// View renders the fleet list page.
func (fp *fleetPage) View(m *model) string {
	return fp.viewFleetList(m)
}

// ===========================================
// Row Management
// ===========================================

// buildRows rebuilds the navigable row list from the current state.
func (fp *fleetPage) buildRows(m *model) {
	wasOnSettings := false
	if r := fp.currentRow(); r != nil && r.kind == rowSettings {
		wasOnSettings = true
	}

	fp.rows = nil

	names := sortedFleetNames(m.st.Fleets)

	for _, name := range names {
		f := m.st.Fleets[name]
		fp.rows = append(fp.rows, row{kind: rowFleetHeader, fleetName: name})
		if !fp.collapsed[name] {
			for _, inst := range f.Instances {
				fp.rows = append(fp.rows, row{kind: rowInstance, fleetName: name, instance: inst})
				instKey := name + "/" + inst.Name
				if m.expandedInstances[instKey] {
					if disc, ok := m.sessions[instKey]; ok && disc.err == nil {
						sanitized := SanitizeSessionName(inst.Name)
						groups := groupSessions(sanitized, disc.sessions)
						for _, g := range groups {
							rootName := g.Sessions[0].Name
							fp.rows = append(fp.rows, row{
								kind:        rowSession,
								fleetName:   name,
								instance:    inst,
								sessionName: rootName,
								groupID:     g.GroupID,
								groupSize:   len(g.Sessions),
							})
						}
					}
					fp.rows = append(fp.rows, row{
						kind:      rowNewSession,
						fleetName: name,
						instance:  inst,
					})
				}
			}
		}
	}
	fp.rows = append(fp.rows, row{kind: rowSettings})
	if wasOnSettings {
		fp.cursor = len(fp.rows) - 1
	}
	if fp.cursor >= len(fp.rows) {
		fp.cursor = max(0, len(fp.rows)-1)
	}
}

// currentRow returns a pointer to the row at the cursor position.
func (fp *fleetPage) currentRow() *row {
	if fp.cursor < 0 || fp.cursor >= len(fp.rows) {
		return nil
	}
	return &fp.rows[fp.cursor]
}

// moveCursor moves the cursor by delta, wrapping around.
func (fp *fleetPage) moveCursor(delta int) {
	if len(fp.rows) == 0 || delta == 0 {
		return
	}
	fp.cursor = (fp.cursor + delta + len(fp.rows)) % len(fp.rows)
}

// moveCursorToInstance moves the cursor to the next (delta > 0) or previous
// (delta < 0) instance row, wrapping around. If the row list contains no
// instance rows, the cursor is left unchanged.
func (fp *fleetPage) moveCursorToInstance(delta int) {
	n := len(fp.rows)
	if n == 0 || delta == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	for range n {
		fp.cursor = (fp.cursor + step + n) % n
		if fp.rows[fp.cursor].kind == rowInstance {
			return
		}
	}
}

// currentFleetName returns the fleet name for the row at the cursor.
func (fp *fleetPage) currentFleetName() string {
	r := fp.currentRow()
	if r == nil || r.kind == rowSettings {
		return ""
	}
	return r.fleetName
}

// selectedInstance returns the fleet and instance when the cursor is
// on an instance row.
func (fp *fleetPage) selectedInstance(m *model) (*fleet.Fleet, *fleet.Instance) {
	r := fp.currentRow()
	if r == nil || r.kind != rowInstance || r.instance == nil {
		return nil, nil
	}
	f := m.st.Fleets[r.fleetName]
	return f, r.instance
}

// selectedSession returns the fleet, instance, and session name when
// the cursor is on a session row.
func (fp *fleetPage) selectedSession(m *model) (*fleet.Fleet, *fleet.Instance, string) {
	r := fp.currentRow()
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
func (fp *fleetPage) updateNormal(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		m.message = ""

		switch msg.String() {
		case "q", "ctrl+c", "ctrl+q":
			m.quitting = true
			return tea.Quit

		case "up", "k":
			fp.moveCursor(-1)

		case "down", "j":
			fp.moveCursor(1)

		case "shift+up", "K":
			fp.moveCursorToInstance(-1)

		case "shift+down", "J":
			fp.moveCursorToInstance(1)

		case " ", "tab":
			if r := fp.currentRow(); r != nil {
				switch r.kind {
				case rowFleetHeader:
					name := r.fleetName
					fp.collapsed[name] = !fp.collapsed[name]
					fp.buildRows(m)
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
						fp.buildRows(m)
					} else {
						m.expandedInstances[instKey] = true
						fp.buildRows(m)
						b := m.instanceBackend(r.instance)
						return listSessionsCmd(b, r.instance.WorkspaceDir, instKey)
					}
				case rowSession, rowNewSession, rowSettings:
					return fp.handleEnter(m)
				}
			}

		case "r":
			if r := fp.currentRow(); r != nil && r.kind == rowSession {
				fp.mode = viewRenameSession
				fp.dialogFleet = r.fleetName
				fp.dialogInst = r.instance.Name
				fp.dialogSession = r.sessionName
				displayName := r.sessionName
				if r.instance != nil {
					sanitized := SanitizeSessionName(r.instance.Name)
					if gid, ok := parseGroupID(sanitized, r.sessionName); ok {
						displayName = gid
					}
				}
				fp.textInput.SetValue(displayName)
				fp.textInput.Placeholder = "new-session-name"
				fp.textInput.CharLimit = 64
				fp.textInput.Focus()
				return fp.textInput.Cursor.BlinkCmd()
			}
			m.reload()
			fp.buildRows(m)
			m.message = "Refreshed"

		case "s":
			r := fp.currentRow()
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
			if !m.instanceBackend(r.instance).Stateful() {
				m.message = fmt.Sprintf("%s instances cannot be stopped or started", backendTypeLabel(r.instance.Backend))
				break
			}

			if r.instance.Status == fleet.StatusRunning {
				r.instance.Status = fleet.StatusStopping
			} else if r.instance.Status == fleet.StatusStopped {
				r.instance.Status = fleet.StatusStarting
			}
			_ = state.Save(m.st)
			fp.buildRows(m)

			fleetName, instName := r.fleetName, r.instance.Name
			return toggleInstanceCmd(fleetName, instName)

		case "d":
			r := fp.currentRow()
			if r == nil || r.kind == rowSettings || r.kind == rowNewSession {
				break
			}
			if r.kind == rowSession {
				fp.dialogFleet = r.fleetName
				fp.dialogInst = r.instance.Name
				fp.dialogSession = r.sessionName
				fp.dialogGroupID = r.groupID
				fp.mode = viewConfirmDeleteSession
				break
			}
			fp.dialogFleet = r.fleetName
			if r.kind == rowFleetHeader {
				fp.dialogInst = ""
			} else if r.instance != nil {
				fp.dialogInst = r.instance.Name
			} else {
				break
			}
			fp.mode = viewConfirmDelete

		case "a":
			r := fp.currentRow()
			if r == nil {
				m.message = "No fleet selected"
				break
			}
			if r.kind == rowInstance || r.kind == rowSession || r.kind == rowNewSession {
				inst := r.instance
				if inst == nil {
					break
				}
				if inst.Status != fleet.StatusRunning {
					m.message = "Instance must be running to create sessions"
					break
				}
				fp.mode = viewCreateSession
				fp.dialogFleet = r.fleetName
				fp.dialogInst = inst.Name
				fp.textInput.SetValue("")
				fp.textInput.Placeholder = "session-name (or empty for auto)"
				fp.textInput.CharLimit = 64
				fp.textInput.Focus()
				return fp.textInput.Cursor.BlinkCmd()
			}
			fleetName := fp.currentFleetName()
			if fleetName == "" {
				m.message = "No fleet selected"
				break
			}
			m.toolStatus = deps.CheckTools()
			available := fp.availableBackendTypes(m)
			if len(available) == 0 {
				m.message = "No deploy targets available – install devcontainer or coder CLI"
				break
			}
			fp.mode = viewAddInstance
			fp.dialogFleet = fleetName
			fp.dialogBackend = available[0]
			if m.cfg != nil {
				preferred := fleet.BackendType(m.cfg.DefaultBackend)
				for _, bt := range available {
					if bt == preferred {
						fp.dialogBackend = preferred
						break
					}
				}
			}
			fp.dialogColor = instanceColorWhite
			fp.dialogRow = addInstanceRowName
			fp.dialogEditing = false
			fp.textInput.SetValue("")
			fp.textInput.Placeholder = "instance-name"
			fp.textInput.CharLimit = 64
			fp.textInput.Focus()
			fp.branchInput.SetValue("")
			fp.branchInput.Placeholder = "default branch"
			fp.branchInput.CharLimit = 128
			fp.branchInput.Blur()
			return fp.textInput.Cursor.BlinkCmd()

		case "n":
			fp.mode = viewAddFleet
			fp.textInput.SetValue("")
			fp.textInput.Placeholder = "git@github.com:org/repo.git"
			fp.textInput.CharLimit = 256
			fp.textInput.Focus()
			return fp.textInput.Cursor.BlinkCmd()

		case "pgup", "pgdown":
			if m.inHostTmux && fp.splitInstance != "" && fp.activeGroupID != "" {
				return fp.cycleSessionGroup(m, msg.String() == "pgup")
			}

		case "enter":
			return fp.handleEnter(m)

		case "e":
			return fp.openEditInstanceDialog(m)

		case "o":
			_, inst := fp.selectedInstance(m)
			if inst == nil {
				m.message = "Select an instance"
				break
			}
			b := m.instanceBackend(inst)
			cmd := b.ExecCommand(inst.WorkspaceDir, freshShellCommand(m.cfg, b.SupportsDotfiles()))
			err := openInTerminal(cmd.Args)
			if err != nil {
				m.message = fmt.Sprintf("Could not open terminal: %v", err)
			} else {
				m.message = fmt.Sprintf("Opened terminal for %s", inst.GetDisplayName())
			}

		case "c":
			_, inst := fp.selectedInstance(m)
			if inst == nil {
				m.message = "Select an instance"
				break
			}
			r := fp.rows[fp.cursor]
			var codeCmd *exec.Cmd
			switch inst.Backend {
			case fleet.BackendCoder:
				codeCmd = exec.Command("coder", backendutil.CoderOpenVSCodeArgs(inst.ContainerID)...)
			case fleet.BackendCodespaces:
				codeCmd = exec.Command("gh", "codespace", "code", "-c", inst.ContainerID)
			default:
				uri, ok := m.instanceBackend(inst).EditorURI(inst.WorkspaceDir, r.fleetName)
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
					m.message = fmt.Sprintf("Opened VS Code for %s", inst.GetDisplayName())
				}
			}

		case "b":
			_, inst := fp.selectedInstance(m)
			if inst == nil {
				m.message = "Select an instance"
				break
			}
			if inst.Status != fleet.StatusRunning {
				m.message = "Instance must be running to open browser"
				break
			}
			b := m.instanceBackend(inst)
			instanceKey := fp.currentFleetName() + "/" + inst.Name
			m.message = fmt.Sprintf("Starting browser proxy for %s...", inst.GetDisplayName())
			return openBrowserProxyCmd(m.portForwards, b, inst, instanceKey)

		case "t":
			_, inst := fp.selectedInstance(m)
			if inst == nil {
				m.message = "Select an instance"
				break
			}
			fp.mode = viewTagInstance
			fp.dialogFleet = fp.currentFleetName()
			fp.dialogInst = inst.Name
			fp.textInput.SetValue(inst.Tag)
			fp.textInput.Placeholder = "short description"
			fp.textInput.CharLimit = 128
			fp.textInput.Focus()
			return fp.textInput.Cursor.BlinkCmd()

		case "l":
			_, inst := fp.selectedInstance(m)
			if inst == nil {
				m.message = "Select an instance"
				break
			}
			r := fp.rows[fp.cursor]
			return tea.ExecProcess(
				logsCommand(m.instanceBackend(inst), r.fleetName, inst),
				func(err error) tea.Msg { return execDoneMsg{err} },
			)

		case "f":
			_, inst := fp.selectedInstance(m)
			if inst == nil {
				m.message = "Select an instance"
				break
			}
			if inst.Status != fleet.StatusRunning {
				m.message = fmt.Sprintf("Instance must be running to port-forward (status: %s)", inst.Status)
				break
			}
			fp.mode = viewPortForward
			fp.dialogFleet = fp.currentFleetName()
			fp.dialogInst = inst.Name
			fp.pfContainerID = inst.ContainerID
			fp.pfCursor = 0
			fp.textInput.SetValue("")
			fp.textInput.Placeholder = "local:remote (e.g. 8080:80)"
			fp.textInput.CharLimit = 11
			fp.textInput.Focus()
			return fp.textInput.Cursor.BlinkCmd()
		}

	case execDoneMsg:
		m.reload()
		fp.buildRows(m)
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
func (fp *fleetPage) handleEnter(m *model) tea.Cmd {
	r := fp.currentRow()
	if r == nil {
		return nil
	}

	switch r.kind {
	case rowSettings:
		m.toolStatus = deps.CheckTools()
		return m.ChangeRoute(routeSettings)

	case rowFleetHeader:
		name := r.fleetName
		fp.collapsed[name] = !fp.collapsed[name]
		fp.buildRows(m)

	case rowNewSession:
		inst := r.instance
		fp.mode = viewCreateSession
		fp.dialogFleet = r.fleetName
		fp.dialogInst = inst.Name
		fp.textInput.SetValue("")
		fp.textInput.Placeholder = "session-name (or empty for auto)"
		fp.textInput.CharLimit = 64
		fp.textInput.Focus()
		return fp.textInput.Cursor.BlinkCmd()

	case rowSession:
		inst := r.instance
		sessionName := r.sessionName
		groupID := r.groupID
		sessInstKey := r.fleetName + "/" + inst.Name
		m.lastActive[sessInstKey] = lastSession{sessionName: sessionName, groupID: groupID}
		if m.inHostTmux {
			if fp.splitPaneID != "" && !splitOpen() {
				unbindHostSplitKeys()
				fp.splitPaneID = ""
				fp.splitFleet = ""
				fp.splitInstance = ""
				fp.splitSession = ""
				fp.activeGroupID = ""
			}
			if fp.splitPaneID != "" && fp.splitInstance == inst.Name && groupID != "" && groupID == fp.activeGroupID {
				fp.saveCurrentGroupLayout(m.st)
				killAllSplitPanes()
				unbindHostSplitKeys()
				fp.splitPaneID = ""
				fp.splitFleet = ""
				fp.splitInstance = ""
				fp.splitSession = ""
				fp.activeGroupID = ""
				return nil
			}
			if fp.splitPaneID != "" && fp.activeGroupID != "" {
				fp.saveCurrentGroupLayout(m.st)
				killAllSplitPanes()
			}
			fp.activeGroupID = groupID
			if groupID != "" && isGroupedSession(SanitizeSessionName(inst.Name), sessionName) {
				return fp.restoreGroupCmd(m, r.fleetName, inst, groupID)
			}
			cols, rows := tmuxWindowSize()
			cols = cols * 70 / 100
			b := m.instanceBackend(inst)
			cmd := b.ExecCommand(
				inst.WorkspaceDir,
				ShellCommandForSession(m.cfg, sessionName, cols, rows, true, b.SupportsDotfiles(), b.SupportsAgentForwarding()),
			)
			return splitPaneCmd(fp.splitPaneID, r.fleetName, inst.Name, sessionName, groupID, cmd)
		}
		b := m.instanceBackend(inst)
		cmd := b.ExecCommand(
			inst.WorkspaceDir,
			ShellCommandForSession(m.cfg, sessionName, m.width, m.height, false, b.SupportsDotfiles(), b.SupportsAgentForwarding()),
		)
		banner := renderGradient(nameToBanner(inst.GetDisplayName()))
		banner += "\n  " + dimStyle.Render("ctrl+q/ctrl+o to detach (session persists)")
		return tea.ExecProcess(
			execWithBannerCmd(banner, cmd),
			func(err error) tea.Msg { return execDoneMsg{err} },
		)

	case rowInstance:
		_, inst := fp.selectedInstance(m)
		if inst == nil {
			break
		}
		instFleetName := r.fleetName
		instKey := instFleetName + "/" + inst.Name
		if m.inHostTmux {
			if fp.splitPaneID != "" && !splitOpen() {
				unbindHostSplitKeys()
				fp.splitPaneID = ""
				fp.splitFleet = ""
				fp.splitInstance = ""
				fp.splitSession = ""
				fp.activeGroupID = ""
			}
			if fp.splitPaneID != "" && fp.splitInstance == inst.Name {
				fp.saveCurrentGroupLayout(m.st)
				killAllSplitPanes()
				unbindHostSplitKeys()
				fp.splitPaneID = ""
				fp.splitFleet = ""
				fp.splitInstance = ""
				fp.splitSession = ""
				fp.activeGroupID = ""
				return nil
			}
			return fp.openInstanceSession(m, instFleetName, inst)
		}

		sessionName := SanitizeSessionName(inst.Name)
		if last, ok := m.lastActive[instKey]; ok {
			sessionName = last.sessionName
		}
		m.lastActive[instKey] = lastSession{sessionName: sessionName}

		b := m.instanceBackend(inst)
		cmd := b.ExecCommand(inst.WorkspaceDir, ShellCommandForSession(m.cfg, sessionName, m.width, m.height, false, b.SupportsDotfiles(), b.SupportsAgentForwarding()))
		banner := renderGradient(nameToBanner(inst.GetDisplayName()))
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
func (fp *fleetPage) contextualHelpKeys(m *model) []string {
	r := fp.currentRow()
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
		if m.inHostTmux && fp.splitInstance != "" && fp.activeGroupID != "" {
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
func (fp *fleetPage) viewFleetList(m *model) string {
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

	for i, r := range fp.rows {
		isSelected := i == fp.cursor
		cursor := "  "
		if isSelected {
			cursor = cursorStyle.Render("> ")
		}

		if r.kind == rowFleetHeader {
			arrow := "▼ "
			style := fleetExpandedStyle
			if fp.collapsed[r.fleetName] {
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
			displayGroupID := fp.activeGroupID
			if fp.pendingGroupID != "" {
				displayGroupID = fp.pendingGroupID
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
			inst := r.instance

			transitional := isTransitional(inst.Status)
			var status string
			if transitional {
				status = strings.TrimRight(m.spinner.View(), "\n") + " " + statusCreatingStyle.Render(string(inst.Status))
			} else {
				status = renderStatus(inst.Status)
			}

			instKey := r.fleetName + "/" + inst.Name
			arrow := "  "
			if inst.Status == fleet.StatusRunning {
				if m.expandedInstances[instKey] {
					arrow = "▼ "
				} else {
					arrow = "▶ "
				}
			}

			paddedName := fmt.Sprintf("%s%-22s", arrow, inst.GetDisplayName())
			switch {
			case isSelected && instanceColorHasCustom(inst.Color):
				paddedName = instanceColorStyle(inst.Color).Bold(true).Render(paddedName)
			case isSelected:
				paddedName = selectedStyle.Render(paddedName)
			case instanceColorHasCustom(inst.Color):
				paddedName = instanceColorStyle(inst.Color).Render(paddedName)
			}

			backendIcon := "⬡"
			switch inst.Backend {
			case fleet.BackendCoder:
				backendIcon = "⌨"
			case fleet.BackendCodespaces:
				backendIcon = "⏣"
			}
			branchItem := ""
			if branch := resolveWorkspaceBranch(inst.WorkspaceDir); branch != "" {
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
				agentStr := ""
				if inst.Status == fleet.StatusRunning && m.activity != nil {
					tool := m.activity.Tool(inst.ContainerID)
					label := agentToolLabel(tool)
					switch m.activity.State(inst.ContainerID) {
					case agentWorking:
						agentStr = agentWorkingStyle.Render(fmt.Sprintf("  \u25b6 %s", label))
					case agentWaiting:
						agentStr = agentWaitingStyle.Render(fmt.Sprintf("  \u23f8 %s", label))
					default:
						agentStr = agentOffStyle.Render("  \u25cb idle")
					}
				}

				statsStr := ""
				if s, ok := m.stats[inst.ContainerID]; ok {
					statsStr = dimStyle.Render(fmt.Sprintf("  %4.0f mcpu  %6.1f MB", s.CPUMillicores, s.MemoryMB))
				}

				line = fmt.Sprintf("%s    %s %s%s%s%s",
					cursor, paddedName, status, agentStr, statsStr, branchItem,
				)

				pfKey := r.fleetName + "/" + inst.Name
				if pfLabel := m.portForwards.FormatLabels(pfKey); pfLabel != "" {
					line += portForwardStyle.Render("  ⇄ " + pfLabel)
				}

				if inst.Tag != "" {
					line += dimStyle.Render("  # " + inst.Tag)
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
	switch fp.mode {
	case viewConfirmDelete:
		b.WriteString("\n")
		var title, body string
		if fp.dialogInst == "" {
			count := 0
			if f, ok := m.st.Fleets[fp.dialogFleet]; ok {
				count = len(f.Instances)
			}
			title = "Delete fleet"
			body = fmt.Sprintf("Remove fleet %s and all %d instance(s)? This will stop all containers and delete all workspaces.", fp.dialogFleet, count)
		} else {
			title = "Delete instance"
			body = fmt.Sprintf("Remove %s/%s? This will stop the container and delete the workspace.", fp.dialogFleet, fp.dialogInst)
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
		if f, ok := m.st.Fleets[fp.dialogFleet]; ok {
			count = len(f.Instances)
		}
		warnDialog := fmt.Sprintf(
			"%s\n\n%s\n\n%s\n\n%s",
			warnBanner.Render("  !! WARNING !!  "),
			dialogLabel.Render(fmt.Sprintf(
				"You are about to destroy fleet %s with %d running instance(s).\nAll containers will be stopped and all workspace data will be permanently deleted.",
				fp.dialogFleet, count,
			)),
			errorStyle.Render("This action cannot be undone."),
			dialogHint.Render("[y] Confirm destroy  [n] Cancel"),
		)
		b.WriteString(warnBox.Render(warnDialog))
		b.WriteString("\n")

	case viewAddInstance:
		b.WriteString("\n")
		bt := fp.dialogBackend
		if bt == "" {
			bt = fleet.BackendDevcontainer
		}
		colorName := fp.dialogColor
		if colorName == "" {
			colorName = instanceColorWhite
		}

		var title, hint, nameField, branchField, deployField string
		if fp.dialogEditing {
			title = "Edit instance"
			hint = "[↑↓] Select  [←→/space] Cycle color  [shift+tab] Color  [enter] Save  [esc] Cancel"
			nameField = fp.textInput.View()
			branchDisplay := fp.branchInput.Value()
			if branchDisplay == "" {
				branchDisplay = "default"
			}
			branchField = dimStyle.Render(branchDisplay)
			deployField = dimStyle.Render(fmt.Sprintf("[ %s ]", backendTypeLabel(bt)))
		} else {
			title = "New instance"
			hint = "[↑↓] Select  [←→/space] Cycle  [shift+tab] Color  [enter] Create  [esc] Cancel"
			if len(fp.availableBackendTypes(m)) > 1 {
				hint = "[↑↓] Select  [←→/space/tab] Cycle  [shift+tab] Color  [enter] Create  [esc] Cancel"
			}
			nameField = fp.textInput.View()
			branchField = fp.branchInput.View()
			deployField = fmt.Sprintf("[ %s ]", backendTypeLabel(bt))
		}

		rowMarker := func(r int) string {
			if !fp.addInstanceRowEnabled(r) {
				return "  "
			}
			if fp.dialogRow == r {
				return cursorStyle.Render("> ")
			}
			return "  "
		}

		colorPreview := instanceColorStyle(colorName).Render(colorName)
		dialog := fmt.Sprintf(
			"%s\n\n  %s %s\n%s%s %s\n%s%s %s\n%s%s [ %s ]\n%s%s %s\n\n%s",
			dialogTitle.Render(title),
			dialogLabel.Render("Fleet:  "),
			fleetExpandedStyle.Render(fp.dialogFleet),
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
			fp.textInput.View(),
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
			fleetExpandedStyle.Render(fp.dialogFleet+"/"+fp.dialogInst),
			dialogLabel.Render("Tag:     "),
			fp.textInput.View(),
			dialogHint.Render("[enter] Save  [esc] Cancel"),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewPortForward:
		b.WriteString("\n")
		pfKey := fp.dialogFleet + "/" + fp.dialogInst
		fwds := m.portForwards.List(pfKey)

		var fwdLines strings.Builder
		if len(fwds) == 0 {
			fwdLines.WriteString(dimStyle.Render("  No active forwards"))
		} else {
			for i, f := range fwds {
				pfCursor := "  "
				if i == fp.pfCursor {
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
			fleetExpandedStyle.Render(fp.dialogFleet+"/"+fp.dialogInst),
			strings.TrimRight(fwdLines.String(), "\n"),
			dialogLabel.Render("Add:"),
			fp.textInput.View(),
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
			fleetExpandedStyle.Render(fp.dialogFleet+"/"+fp.dialogInst),
			dialogLabel.Render("Name:    "),
			fp.textInput.View(),
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
			fleetExpandedStyle.Render(fp.dialogFleet+"/"+fp.dialogInst),
			dialogLabel.Render("Current: "),
			sessionStyle.Render(fp.dialogSession),
			dialogLabel.Render("New:     "),
			fp.textInput.View(),
			dialogHint.Render("[enter] Rename  [esc] Cancel"),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")

	case viewConfirmDeleteSession:
		b.WriteString("\n")
		displayName := fp.dialogSession
		if fp.dialogGroupID != "" {
			displayName = fp.dialogGroupID
		}
		dialog := fmt.Sprintf(
			"%s\n\n%s\n\n%s",
			dialogTitle.Render("Delete session"),
			dialogLabel.Render(fmt.Sprintf("Remove session %s from %s/%s?",
				displayName, fp.dialogFleet, fp.dialogInst)),
			dialogHint.Render("[y] Yes  [n] No"),
		)
		b.WriteString(dialogBox.Render(dialog))
		b.WriteString("\n")
	}

	if m.message != "" {
		b.WriteString(messageStyle.Render(m.message))
		b.WriteString("\n")
	}

	if m.cfg == nil || m.cfg.GeneralSettings.ShowHelpTextEnabled() {
		b.WriteString(renderHelp(m.width, fp.contextualHelpKeys(m)))
	}

	return b.String()
}

// ===========================================
// Session Management
// ===========================================

// openInstanceSession opens a split pane for the given instance, reusing
// the last active session when available.
func (fp *fleetPage) openInstanceSession(m *model, fleetName string, inst *fleet.Instance) tea.Cmd {
	instKey := fleetName + "/" + inst.Name
	sanitized := SanitizeSessionName(inst.Name)

	// The session discovery loop only runs for expanded instances, so
	// hitting enter on a collapsed row with no lastActive entry would
	// otherwise always spawn a new group. Load sessions on demand here
	// so we can attach to an existing one when available.
	ensureSessionsLoaded(m, m.instanceBackend(inst), inst.WorkspaceDir, instKey)

	if last, ok := m.lastActive[instKey]; ok {
		if last.groupID != "" {
			return fp.restoreGroupCmd(m, fleetName, inst, last.groupID)
		}
		cols, rows := tmuxWindowSize()
		cols = cols * 70 / 100
		b := m.instanceBackend(inst)
		cmd := b.ExecCommand(
			inst.WorkspaceDir,
			ShellCommandForSession(m.cfg, last.sessionName, cols, rows, true, b.SupportsDotfiles(), b.SupportsAgentForwarding()),
		)
		return splitPaneCmd(fp.splitPaneID, fleetName, inst.Name, last.sessionName, last.groupID, cmd)
	}

	if disc, ok := m.sessions[instKey]; ok && disc.err == nil && len(disc.sessions) > 0 {
		groups := groupSessions(sanitized, disc.sessions)
		if len(groups) > 0 {
			g := groups[0]
			rootName := g.Sessions[0].Name
			if g.GroupID != "" && isGroupedSession(sanitized, rootName) {
				return fp.restoreGroupCmd(m, fleetName, inst, g.GroupID)
			}
			cols, rows := tmuxWindowSize()
			cols = cols * 70 / 100
			b := m.instanceBackend(inst)
			cmd := b.ExecCommand(
				inst.WorkspaceDir,
				ShellCommandForSession(m.cfg, rootName, cols, rows, true, b.SupportsDotfiles(), b.SupportsAgentForwarding()),
			)
			return splitPaneCmd(fp.splitPaneID, fleetName, inst.Name, rootName, g.GroupID, cmd)
		}
	}

	newGroupID := randomHex(3)
	sessName := groupSessionName(sanitized, newGroupID)
	cols, rows := tmuxWindowSize()
	cols = cols * 70 / 100
	b := m.instanceBackend(inst)
	cmd := b.ExecCommand(
		inst.WorkspaceDir,
		ShellCommandForSession(m.cfg, sessName, cols, rows, true, b.SupportsDotfiles(), b.SupportsAgentForwarding()),
	)
	return splitPaneCmd(fp.splitPaneID, fleetName, inst.Name, sessName, newGroupID, cmd)
}

// instanceGroups returns the session groups for the given instance name.
func (fp *fleetPage) instanceGroups(m *model, instanceName string) []sessionGroup {
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
func (fp *fleetPage) cycleSessionGroup(m *model, prev bool) tea.Cmd {
	groups := fp.instanceGroups(m, fp.splitInstance)
	if len(groups) < 2 {
		return nil
	}

	fromID := fp.activeGroupID
	if fp.pendingGroupID != "" {
		fromID = fp.pendingGroupID
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

	fp.pendingGroupID = groups[targetIdx].GroupID
	fp.debounceSeq++
	return groupCycleDebounce(fp.debounceSeq)
}

// commitGroupCycle performs the actual pane switch after the debounce
// timer expires.
func (fp *fleetPage) commitGroupCycle(m *model) tea.Cmd {
	if fp.pendingGroupID == "" || fp.pendingGroupID == fp.activeGroupID {
		fp.pendingGroupID = ""
		return nil
	}

	var inst *fleet.Instance
	for _, f := range m.st.Fleets {
		for _, i := range f.Instances {
			if i.Name == fp.splitInstance {
				inst = i
				break
			}
		}
		if inst != nil {
			break
		}
	}
	if inst == nil {
		fp.pendingGroupID = ""
		return nil
	}

	targetGroupID := fp.pendingGroupID
	fp.pendingGroupID = ""

	fp.saveCurrentGroupLayout(m.st)
	killAllSplitPanes()

	fp.activeGroupID = targetGroupID

	return fp.restoreGroupCmd(m, fp.splitFleet, inst, targetGroupID)
}

// ===========================================
// Backend Helpers
// ===========================================

// availableBackendTypes returns the subset of backend types whose
// required CLI tool is found on the system.
func (fp *fleetPage) availableBackendTypes(m *model) []fleet.BackendType {
	var out []fleet.BackendType
	for _, bt := range allBackendTypes {
		bin := backendToolRequirements[bt]
		if bin == "" {
			out = append(out, bt)
			continue
		}
		for _, t := range m.toolStatus {
			if t.Binary == bin && t.Found {
				out = append(out, bt)
				break
			}
		}
	}
	return out
}
