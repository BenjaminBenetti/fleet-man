package tui

import (
	"encoding/hex"
	"fmt"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/internal/deps"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	tea "github.com/charmbracelet/bubbletea"
)

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
			fleetPage.rightSelected = false
			if fleetPage.cursor == fleetPage.firstSelectable() {
				fleetPage.armadaSel.focused = true
			} else {
				fleetPage.moveCursor(-1)
			}

		case "down", "j":
			// Down from the bottom row wraps up to the Armada selector.
			fleetPage.rightSelected = false
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
			fleetPage.rightSelected = false
			fleetPage.moveCursorToInstance(-1)

		case "shift+down", "J":
			fleetPage.rightSelected = false
			fleetPage.moveCursorToInstance(1)

		case " ", "tab":
			// When a right-hand element is selected the cursor's normal space/tab
			// action (connect/create a session, expand/collapse a header) is
			// intercepted to activate that element instead — matching enter and the
			// focused help text.
			if fleetPage.rightSelected {
				return fleetPage.activateRightSelection(m)
			}
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
				case rowSession, rowNewSession, rowSettings, rowLeaveFocus,
					rowAutomationTriggers, rowAutomationAgents, rowTrigger, rowAgent,
					rowNewTrigger, rowNewAgent:
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
			if r.kind == rowTrigger {
				return fleetPage.deleteTrigger(m, r.fleetName, r.autoIdx)
			}
			if r.kind == rowAgent {
				return fleetPage.deleteAgent(m, r.fleetName, r.autoIdx)
			}
			if r.kind == rowAutomationTriggers || r.kind == rowAutomationAgents || r.kind == rowNewTrigger || r.kind == rowNewAgent {
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
			// A selected right-hand element intercepts enter to activate itself
			// (open the PR, or flip the mode toggle); otherwise the row's normal
			// action runs.
			if fleetPage.rightSelected {
				return fleetPage.activateRightSelection(m)
			}
			return fleetPage.handleEnter(m)

		case "e":
			if r := fleetPage.currentRow(); r != nil {
				switch r.kind {
				case rowFleetHeader:
					return fleetPage.openEditFleetDialog(m)
				case rowTrigger:
					return fleetPage.openEditTriggerDialog(m, r.fleetName, r.autoIdx)
				case rowAgent:
					return fleetPage.openEditAgentDialog(m, r.fleetName, r.autoIdx)
				case rowAutomationTriggers, rowAutomationAgents, rowNewTrigger, rowNewAgent:
					return nil
				}
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

		case "right", "l":
			// Select the current row's right-hand element — a fleet header's
			// [automations]/[instances] mode toggle, or an expanded instance's
			// inline PR status — so enter activates it. No-op on rows that carry
			// no such element.
			if fleetPage.currentRowHasRightElement(m) {
				fleetPage.rightSelected = true
			}

		case "left", "h":
			fleetPage.rightSelected = false

		case "L":
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

		case "m":
			// Toggle the fleet between its instance view and its automation view
			// (triggers + agents). Works from any row belonging to a fleet.
			name := fleetPage.currentFleetName()
			if name == "" {
				m.message = "Select a fleet to toggle automation"
				break
			}
			fleetPage.toggleAutomationMode(m, name)

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

	case rowAutomationTriggers:
		fleetPage.autoCollapsed["trig:"+r.fleetName] = !fleetPage.autoCollapsed["trig:"+r.fleetName]
		fleetPage.buildRows(m)

	case rowAutomationAgents:
		fleetPage.autoCollapsed["agent:"+r.fleetName] = !fleetPage.autoCollapsed["agent:"+r.fleetName]
		fleetPage.buildRows(m)

	case rowTrigger:
		return fleetPage.openEditTriggerDialog(m, r.fleetName, r.autoIdx)

	case rowNewTrigger:
		return fleetPage.openAddTriggerDialog(m, r.fleetName)

	case rowAgent:
		return fleetPage.openEditAgentDialog(m, r.fleetName, r.autoIdx)

	case rowNewAgent:
		return fleetPage.openAddAgentDialog(m, r.fleetName)

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

// activateRightSelection runs the action of the selected right-hand element on
// the cursor row: flipping automation mode on a fleet header (keeping the toggle
// focused so enter flips back), or opening the PR on a PR-carrying child row.
// Mirrors the row-kind split in currentRowHasRightElement.
func (fleetPage *fleetPage) activateRightSelection(m *model) tea.Cmd {
	r := fleetPage.currentRow()
	if r == nil {
		return nil
	}
	if r.kind == rowFleetHeader {
		fleetPage.toggleAutomationMode(m, r.fleetName)
		fleetPage.rightSelected = true // keep the toggle focused for repeat flips
		return nil
	}
	return fleetPage.openSelectedPR(m)
}

// ===========================================
// Help Keys
// ===========================================

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

	// A selected right-hand element puts the row in a focused sub-mode: only the
	// activate/deselect keys apply. The two such elements are a fleet header's
	// mode toggle and an instance's inline PR status.
	if fleetPage.rightSelected {
		if r.kind == rowFleetHeader {
			action := "enter: automations"
			if fleetPage.automationMode[r.fleetName] {
				action = "enter: instances"
			}
			return withArmadaHint([]string{action, "←/h: deselect", "j/k: navigate", "q: quit"})
		}
		return withArmadaHint([]string{"enter: open PR", "←/h: deselect", "j/k: navigate", "q: quit"})
	}

	switch r.kind {
	case rowFleetHeader:
		toggle := "m: automations"
		if fleetPage.automationMode[r.fleetName] {
			toggle = "m: instances"
		}
		return withArmadaHint([]string{
			"j/k: navigate", "→/l: select", "space/enter: expand/collapse", toggle, "e: edit fleet",
			"a: add instance", "n: new fleet", "d: delete fleet", "r: refresh", "q: quit",
		})

	case rowAutomationTriggers, rowAutomationAgents:
		return withArmadaHint([]string{
			"j/k: navigate", "space/enter: expand/collapse", "m: instances", "q: quit",
		})

	case rowTrigger, rowAgent:
		return withArmadaHint([]string{
			"j/k: navigate", "enter/e: edit", "d: delete", "m: instances", "q: quit",
		})

	case rowNewTrigger, rowNewAgent:
		return withArmadaHint([]string{
			"j/k: navigate", "enter: add", "m: instances", "q: quit",
		})

	case rowInstance:
		keys := []string{"j/k: navigate"}
		if r.instance != nil {
			switch {
			case r.instance.Status == fleet.StatusRunning:
				keys = append(keys,
					"space: show sessions", "enter: open shell", "e: edit",
					"s: stop", "a: new session", "d: delete", "t: tag",
					"p: port-forward", "b: browser", "c: code", "C: clone", "R: rebuild", "o: terminal", "L: logs",
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
		keys := []string{"j/k: navigate"}
		if len(m.rowInlinePRRefs(*r)) > 0 {
			keys = append(keys, "→/l: select PR")
		}
		keys = append(keys,
			"space/enter/e: connect",
			"a: new session", "d: delete session", "r: rename", "q: quit",
		)
		if m.inHostTmux && fleetPage.split.ref.Valid() && !fleetPage.split.activeGroup.Empty() {
			keys = append(keys[:len(keys)-1], "pgup/pgdn: cycle groups", "q: quit")
		}
		return withArmadaHint(keys)

	case rowNewSession:
		keys := []string{"j/k: navigate"}
		if len(m.rowInlinePRRefs(*r)) > 0 {
			keys = append(keys, "→/l: select PR")
		}
		keys = append(keys, "space/enter/e: create session", "a: new session", "q: quit")
		return withArmadaHint(keys)

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
