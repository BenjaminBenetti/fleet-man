package tui

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/devcontainersetup"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
	devcontainercheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/devcontainer"
	homedircheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/homedir"
	tea "github.com/charmbracelet/bubbletea"
)

// ===========================================
// Delete Dialogs
// ===========================================

// updateConfirmDelete handles the instance/fleet deletion confirmation dialog.
func (fleetPage *fleetPage) updateConfirmDelete(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y", "enter":
			if fleetPage.dialogInst == "" {
				// Fleet-level delete — check if it has instances for double confirm
				if f, ok := m.st.Fleets[fleetPage.dialogFleet]; ok && len(f.Instances) > 0 {
					fleetPage.mode = viewConfirmDeleteFleetWarn
					return nil
				}
				// Empty fleet, just remove it
				delete(m.st.Fleets, fleetPage.dialogFleet)
				delete(fleetPage.collapsed, fleetPage.dialogFleet)
				_ = destroyFleetRemote(fleetPage.dialogFleet)
				fleetPage.buildRows(m)
				m.message = fmt.Sprintf("Removed fleet %s", fleetPage.dialogFleet)
			} else {
				// Instance-level delete runs as a server job. Flip an optimistic
				// in-memory Deleting status for the spinner (NOT persisted — the
				// server owns the teardown and the record removal).
				f, ok := m.st.Fleets[fleetPage.dialogFleet]
				if ok {
					instance, err := f.GetInstance(fleetPage.dialogInst)
					if err == nil {
						instance.Status = fleet.StatusDeleting
						fleetPage.buildRows(m)
						fleetPage.mode = viewNormal
						return deleteInstanceCmd(fleetPage.dialogFleet, fleetPage.dialogInst, m.portForwards)
					}
				}
			}
			fleetPage.mode = viewNormal

		case "n", "N", "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			fleetPage.blurDialogFields()
			m.message = "Cancelled"
		}
	}
	return nil
}

// updateConfirmDeleteFleetWarn handles the double-confirm dialog for
// fleets with running instances.
func (fleetPage *fleetPage) updateConfirmDeleteFleetWarn(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y", "enter":
			f, ok := m.st.Fleets[fleetPage.dialogFleet]
			if ok && len(f.Instances) > 0 {
				for _, instance := range f.Instances {
					instance.Status = fleet.StatusDeleting // optimistic, in-memory only
				}
				fleetPage.buildRows(m)
				fleetPage.mode = viewNormal
				return deleteFleetCmd(fleetPage.dialogFleet, f.Instances, m.portForwards)
			} else if ok {
				delete(m.st.Fleets, fleetPage.dialogFleet)
				delete(fleetPage.collapsed, fleetPage.dialogFleet)
				_ = destroyFleetRemote(fleetPage.dialogFleet)
				fleetPage.buildRows(m)
				m.message = fmt.Sprintf("Removed fleet %s", fleetPage.dialogFleet)
			}
			fleetPage.mode = viewNormal

		case "n", "N", "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			fleetPage.blurDialogFields()
			m.message = "Cancelled"
		}
	}
	return nil
}

// updateConfirmBrowserSwitch handles the prompt shown when the user asks
// to open a browser for an instance but another browser is already
// running against the same user-data-dir. Default action is "yes,
// switch": Chrome's singleton lock means the existing process must be
// killed before a fresh launch with this instance's proxy can succeed.
//
// Once the user confirms, the dialog stays open but its body swaps to a
// "Switching..." spinner — the kill+relaunch cmd runs asynchronously and
// the dialog is torn down when browserProxyMsg returns.
func (fleetPage *fleetPage) updateConfirmBrowserSwitch(m *model, msg tea.Msg) tea.Cmd {
	// While the kill+relaunch is in flight, swallow input so the user
	// can't double-trigger the flow.
	if fleetPage.dialogBrowserSwitching {
		return nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "n", "N", "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			m.message = "Cancelled"
			return nil

		// Anything else (y/Y/enter/space) confirms — default-yes
		// matches the issue spec, so we treat unrecognised keys as
		// "proceed" rather than swallowing them.
		default:
			fleetName := fleetPage.dialogFleet
			instanceName := fleetPage.dialogInst

			f, ok := m.st.Fleets[fleetName]
			if !ok {
				fleetPage.mode = viewNormal
				m.message = "Fleet no longer exists"
				return nil
			}
			instance, err := f.GetInstance(instanceName)
			if err != nil {
				fleetPage.mode = viewNormal
				m.message = fmt.Sprintf("Instance no longer exists: %v", err)
				return nil
			}
			if instance.Status != fleet.StatusRunning {
				fleetPage.mode = viewNormal
				m.message = "Instance must be running to open browser"
				return nil
			}

			dataDir := browserDataDir(fleetName, instanceName, multipleBrowsersPerFleet(m))
			instanceBackend := m.instanceBackend(instance)
			instanceKey := fleetName + "/" + instanceName

			// Switch the dialog into "in-flight" mode; the renderer
			// will draw the spinner. The cmd does the kill + relaunch
			// in the background and the resulting browserProxyMsg
			// clears the flag and the mode.
			fleetPage.dialogBrowserSwitching = true
			m.message = ""
			return switchBrowserCmd(m.portForwards, instanceBackend, instance, instanceKey, dataDir, f.Settings.PreferFleetLaunchEnabled(), "")
		}
	}
	return nil
}

// chooseBrowserRow identifies a selectable option in the
// choose-browser-launch dialog.
const (
	chooseBrowserRowFleetLaunch = iota
	chooseBrowserRowInitialURL
	chooseBrowserRowCount
)

// updateChooseBrowserLaunch handles the dialog shown the first time the
// browser is opened on a fleet whose workspace configures both an
// initialUrl and a Fleet Launch landing page. Navigation matches the rest
// of fleet — j/k or arrows move the cursor, enter/space chooses the
// selected row — and the [f]/[u] shortcuts jump straight to a choice. The
// answer is saved as the fleet's PreferFleetLaunch setting and the browser
// then launches.
func (fleetPage *fleetPage) updateChooseBrowserLaunch(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch keyMsg.String() {
	case "esc", "q", "Q", "ctrl+c":
		fleetPage.mode = viewNormal
		m.message = "Cancelled"
		return nil
	case "up", "k":
		fleetPage.dialogRow = (fleetPage.dialogRow - 1 + chooseBrowserRowCount) % chooseBrowserRowCount
		return nil
	case "down", "j", "tab":
		fleetPage.dialogRow = (fleetPage.dialogRow + 1) % chooseBrowserRowCount
		return nil
	case "enter", " ":
		return fleetPage.chooseBrowserLaunch(m, fleetPage.dialogRow == chooseBrowserRowFleetLaunch)
	case "f", "F":
		return fleetPage.chooseBrowserLaunch(m, true)
	case "u", "U":
		return fleetPage.chooseBrowserLaunch(m, false)
	}
	return nil
}

// chooseBrowserLaunch persists the browser-start preference for the fleet
// and proceeds with the launch using it.
func (fleetPage *fleetPage) chooseBrowserLaunch(m *model, preferFleetLaunch bool) tea.Cmd {
	fleetName := fleetPage.dialogFleet
	f, ok := m.st.Fleets[fleetName]
	if !ok {
		fleetPage.mode = viewNormal
		m.message = "Fleet no longer exists"
		return nil
	}

	prefer := preferFleetLaunch
	f.Settings.PreferFleetLaunch = &prefer
	_ = setFleetSettingsRemote(fleetName, f.Settings)

	instance, err := f.GetInstance(fleetPage.dialogInst)
	if err != nil || instance.Status != fleet.StatusRunning {
		fleetPage.mode = viewNormal
		m.message = "Instance must be running to open browser"
		return nil
	}

	fleetPage.mode = viewNormal
	return fleetPage.startBrowser(m, instance, fleetName)
}

// updateConfirmDeleteSession handles the session deletion confirmation dialog.
func (fleetPage *fleetPage) updateConfirmDeleteSession(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y", "enter":
			fleetPage.mode = viewNormal
			ref := InstanceRef{Fleet: fleetPage.dialogFleet, Instance: fleetPage.dialogInst}
			f, ok := m.st.Fleets[fleetPage.dialogFleet]
			if !ok {
				break
			}
			instance, err := f.GetInstance(fleetPage.dialogInst)
			if err != nil {
				break
			}
			instanceBackend := m.instanceBackend(instance)
			sanitized := SanitizeSessionName(instance.Name)
			if fleetPage.dialogGroupID != "" && isGroupedSession(sanitized, fleetPage.dialogSession) {
				return deleteGroupSessionsCmd(instanceBackend, instance.WorkspaceDir, ref, sanitized, fleetPage.dialogGroupID)
			}
			return deleteSessionCmd(instanceBackend, instance.WorkspaceDir, ref, fleetPage.dialogSession)

		case "n", "N", "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			fleetPage.blurDialogFields()
			m.message = "Cancelled"
		}
	}
	return nil
}

// ===========================================
// Backend Type Helpers
// ===========================================

// backendToolRequirements maps each backend type to the CLI binary it
// requires. An empty string means no external tool is needed.
var backendToolRequirements = map[fleet.BackendType]string{
	fleet.BackendDevcontainer: "devcontainer",
	fleet.BackendCoder:        "coder",
	fleet.BackendCodespaces:   "gh",
}

// allBackendTypes is the ordered master list of every backend type.
var allBackendTypes = []fleet.BackendType{
	fleet.BackendDevcontainer,
	fleet.BackendCoder,
	fleet.BackendCodespaces,
}

// nextBackendType cycles through the given options list from current.
func nextBackendType(current fleet.BackendType, direction int, options []fleet.BackendType) fleet.BackendType {
	if len(options) == 0 {
		return current
	}
	idx := 0
	for i, backendType := range options {
		if backendType == current {
			idx = i
			break
		}
	}
	idx = (idx + direction + len(options)) % len(options)
	return options[idx]
}

// backendTypeLabel returns a human-readable label for a backend type.
func backendTypeLabel(backendType fleet.BackendType) string {
	switch backendType {
	case fleet.BackendCoder:
		return "Coder"
	case fleet.BackendCodespaces:
		return "Codespaces"
	default:
		return "Devcontainer"
	}
}

func isDialogUpKey(key string) bool {
	return key == "up" || key == "k"
}

func isDialogDownKey(key string) bool {
	return key == "down" || key == "j"
}

func isDialogLeftKey(key string) bool {
	return key == "left" || key == "h"
}

func isDialogRightKey(key string) bool {
	return key == "right" || key == "l"
}

func isDialogTextKey(msg tea.KeyMsg) bool {
	if msg.Alt {
		return false
	}
	switch msg.Type {
	case tea.KeyBackspace, tea.KeyDelete:
		return true
	}
	if msg.Type != tea.KeyRunes || len(msg.Runes) == 0 {
		return false
	}
	switch msg.String() {
	case "h", "j", "k", "l", "q", "Q", "d", "x":
		return false
	default:
		return true
	}
}

func (fleetPage *fleetPage) blurDialogFields() {
	fleetPage.dialogFieldActive = false
	fleetPage.textInput.Blur()
	fleetPage.branchInput.Blur()
	fleetPage.homedirInput.Blur()
}

func (fleetPage *fleetPage) activateTextInput() tea.Cmd {
	fleetPage.dialogFieldActive = true
	fleetPage.textInput.Focus()
	return fleetPage.textInput.Cursor.BlinkCmd()
}

func (fleetPage *fleetPage) deactivateTextInput() {
	fleetPage.dialogFieldActive = false
	fleetPage.textInput.Blur()
}

func (fleetPage *fleetPage) activateTextInputWithMsg(msg tea.Msg) tea.Cmd {
	blinkCmd := fleetPage.activateTextInput()
	var inputCmd tea.Cmd
	fleetPage.textInput, inputCmd = fleetPage.textInput.Update(msg)
	return tea.Batch(blinkCmd, inputCmd)
}

// ===========================================
// Add Instance Dialog
// ===========================================

// addInstanceRow identifies a focusable row in the add-instance dialog.
const (
	addInstanceRowName = iota
	addInstanceRowBranch
	addInstanceRowColor
	addInstanceRowDeploy
	addInstanceRowCount
)

// openEditInstanceDialog opens the add-instance dialog in edit mode for
// the currently selected instance. The user-facing Name (stored as
// DisplayName) and color are editable; the underlying identifier, branch,
// and deploy target are immutable — they describe how the workspace was
// originally provisioned.
func (fleetPage *fleetPage) openEditInstanceDialog(m *model) tea.Cmd {
	f, instance := fleetPage.selectedInstance(m)
	if instance == nil || f == nil {
		m.message = "Select an instance to edit"
		return nil
	}

	fleetPage.mode = viewAddInstance
	fleetPage.dialogEditing = true
	fleetPage.dialogFleet = f.Name
	fleetPage.dialogInst = instance.Name
	fleetPage.dialogBackend = instance.Backend
	if fleetPage.dialogBackend == "" {
		fleetPage.dialogBackend = fleet.BackendDevcontainer
	}
	fleetPage.dialogColor = instance.Color
	if fleetPage.dialogColor == "" {
		fleetPage.dialogColor = instanceColorWhite
	}
	fleetPage.dialogRow = addInstanceRowName
	fleetPage.dialogFieldActive = false
	fleetPage.textInput.SetValue(instance.GetDisplayName())
	fleetPage.branchInput.SetValue(instance.Branch)
	fleetPage.syncAddInstanceFocus()
	return nil
}

// updateAddInstance handles the add-instance dialog.
func (fleetPage *fleetPage) updateAddInstance(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dialogFieldActive {
			switch msg.String() {
			case "enter":
				return fleetPage.submitAddInstance(m)
			case "esc":
				fleetPage.dialogFieldActive = false
				fleetPage.syncAddInstanceFocus()
				return nil
			case "ctrl+c":
				return fleetPage.cancelAddInstance(m)
			}
			return fleetPage.updateActiveAddInstanceField(msg)
		}

		switch msg.String() {
		case "enter":
			if fleetPage.dialogRow == addInstanceRowName || (fleetPage.dialogRow == addInstanceRowBranch && !fleetPage.dialogEditing) {
				return fleetPage.activateAddInstanceField()
			}
			return fleetPage.submitAddInstance(m)

		case "tab":
			if fleetPage.dialogEditing {
				return nil
			}
			opts := fleetPage.availableBackendTypes(m)
			if len(opts) > 1 {
				fleetPage.dialogBackend = nextBackendType(fleetPage.dialogBackend, 1, opts)
			}
			return nil

		case "shift+tab":
			fleetPage.dialogColor = nextInstanceColor(fleetPage.dialogColor, 1)
			return nil

		case "up":
			fleetPage.dialogFieldActive = false
			fleetPage.dialogRow = fleetPage.prevAddInstanceRow(fleetPage.dialogRow)
			fleetPage.syncAddInstanceFocus()
			return nil

		case "down":
			fleetPage.dialogFieldActive = false
			fleetPage.dialogRow = fleetPage.nextAddInstanceRow(fleetPage.dialogRow)
			fleetPage.syncAddInstanceFocus()
			return nil

		case "left":
			if fleetPage.dialogRow == addInstanceRowDeploy && !fleetPage.dialogEditing {
				opts := fleetPage.availableBackendTypes(m)
				if len(opts) > 1 {
					fleetPage.dialogBackend = nextBackendType(fleetPage.dialogBackend, -1, opts)
				}
				return nil
			}
			if fleetPage.dialogRow == addInstanceRowColor {
				fleetPage.dialogColor = nextInstanceColor(fleetPage.dialogColor, -1)
				return nil
			}

		case "right", " ":
			if msg.String() == " " && (fleetPage.dialogRow == addInstanceRowName || (fleetPage.dialogRow == addInstanceRowBranch && !fleetPage.dialogEditing)) {
				return fleetPage.activateAddInstanceField()
			}
			if fleetPage.dialogRow == addInstanceRowDeploy && !fleetPage.dialogEditing {
				opts := fleetPage.availableBackendTypes(m)
				if len(opts) > 1 {
					fleetPage.dialogBackend = nextBackendType(fleetPage.dialogBackend, 1, opts)
				}
				return nil
			}
			if fleetPage.dialogRow == addInstanceRowColor {
				fleetPage.dialogColor = nextInstanceColor(fleetPage.dialogColor, 1)
				return nil
			}

		case "esc", "q", "Q", "ctrl+c":
			return fleetPage.cancelAddInstance(m)
		}

		if isDialogUpKey(msg.String()) {
			fleetPage.dialogFieldActive = false
			fleetPage.dialogRow = fleetPage.prevAddInstanceRow(fleetPage.dialogRow)
			fleetPage.syncAddInstanceFocus()
			return nil
		}
		if isDialogDownKey(msg.String()) {
			fleetPage.dialogFieldActive = false
			fleetPage.dialogRow = fleetPage.nextAddInstanceRow(fleetPage.dialogRow)
			fleetPage.syncAddInstanceFocus()
			return nil
		}
		if isDialogLeftKey(msg.String()) {
			if fleetPage.dialogRow == addInstanceRowDeploy && !fleetPage.dialogEditing {
				opts := fleetPage.availableBackendTypes(m)
				if len(opts) > 1 {
					fleetPage.dialogBackend = nextBackendType(fleetPage.dialogBackend, -1, opts)
				}
				return nil
			}
			if fleetPage.dialogRow == addInstanceRowColor {
				fleetPage.dialogColor = nextInstanceColor(fleetPage.dialogColor, -1)
				return nil
			}
		}
		if isDialogRightKey(msg.String()) {
			if fleetPage.dialogRow == addInstanceRowDeploy && !fleetPage.dialogEditing {
				opts := fleetPage.availableBackendTypes(m)
				if len(opts) > 1 {
					fleetPage.dialogBackend = nextBackendType(fleetPage.dialogBackend, 1, opts)
				}
				return nil
			}
			if fleetPage.dialogRow == addInstanceRowColor {
				fleetPage.dialogColor = nextInstanceColor(fleetPage.dialogColor, 1)
				return nil
			}
		}
		if isDialogTextKey(msg) && (fleetPage.dialogRow == addInstanceRowName || (fleetPage.dialogRow == addInstanceRowBranch && !fleetPage.dialogEditing)) {
			return fleetPage.activateAddInstanceFieldWithMsg(msg)
		}
	}

	return nil
}

func (fleetPage *fleetPage) updateActiveAddInstanceField(msg tea.Msg) tea.Cmd {
	switch fleetPage.dialogRow {
	case addInstanceRowName:
		var cmd tea.Cmd
		fleetPage.textInput, cmd = fleetPage.textInput.Update(msg)
		return cmd
	case addInstanceRowBranch:
		var cmd tea.Cmd
		fleetPage.branchInput, cmd = fleetPage.branchInput.Update(msg)
		return cmd
	}
	return nil
}

func (fleetPage *fleetPage) submitAddInstance(m *model) tea.Cmd {
	if fleetPage.dialogEditing {
		return fleetPage.saveInstanceEdits(m)
	}
	name := strings.TrimSpace(fleetPage.textInput.Value())
	if name == "" {
		m.message = "Name cannot be empty"
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}

	fleetName := fleetPage.dialogFleet
	f, ok := m.st.Fleets[fleetName]
	if !ok {
		m.message = fmt.Sprintf("Fleet %s not found", fleetName)
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}

	if _, err := f.GetInstance(name); err == nil {
		m.message = fmt.Sprintf("Instance %s/%s already exists", fleetName, name)
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}

	backendType := fleetPage.dialogBackend
	if backendType == "" {
		backendType = fleet.BackendDevcontainer
	}

	color := fleetPage.dialogColor
	if color == instanceColorWhite {
		color = ""
	}

	branch := strings.TrimSpace(fleetPage.branchInput.Value())

	// Record the chosen backend as the new default. The instance record itself
	// is pre-created server-side by the CreateInstance job (no client-side state
	// write — the #63 fix); instanceSpawnedMsg reload()s it into view.
	if m.config != nil {
		m.config.DefaultBackend = string(backendType)
		_ = setConfigRemote(m.config)
	}

	key := fleetName + "/" + name
	m.creating[key] = true
	fleetPage.mode = viewNormal
	fleetPage.blurDialogFields()
	m.message = fmt.Sprintf("Creating %s (%s)...", key, backendTypeLabel(backendType))

	return createInstanceCmd(fleetName, name, f.Remote, branch, color, backendType)
}

func (fleetPage *fleetPage) cancelAddInstance(m *model) tea.Cmd {
	fleetPage.mode = viewNormal
	fleetPage.dialogEditing = false
	fleetPage.blurDialogFields()
	m.message = "Cancelled"
	return nil
}

// syncAddInstanceFocus focuses the text input of the currently selected
// row so the cursor visually reflects the current focus. In edit mode
// the branch input is immutable so it never receives focus; the name
// input edits DisplayName and stays focusable.
func (fleetPage *fleetPage) syncAddInstanceFocus() {
	nameFocus := fleetPage.dialogFieldActive && fleetPage.dialogRow == addInstanceRowName
	branchFocus := fleetPage.dialogFieldActive && fleetPage.dialogRow == addInstanceRowBranch && !fleetPage.dialogEditing

	if nameFocus {
		fleetPage.textInput.Focus()
	} else {
		fleetPage.textInput.Blur()
	}

	if branchFocus {
		fleetPage.branchInput.Focus()
	} else {
		fleetPage.branchInput.Blur()
	}
}

func (fleetPage *fleetPage) activateAddInstanceField() tea.Cmd {
	fleetPage.dialogFieldActive = true
	fleetPage.syncAddInstanceFocus()
	switch fleetPage.dialogRow {
	case addInstanceRowName:
		return fleetPage.textInput.Cursor.BlinkCmd()
	case addInstanceRowBranch:
		if !fleetPage.dialogEditing {
			return fleetPage.branchInput.Cursor.BlinkCmd()
		}
	}
	fleetPage.dialogFieldActive = false
	fleetPage.syncAddInstanceFocus()
	return nil
}

func (fleetPage *fleetPage) activateAddInstanceFieldWithMsg(msg tea.Msg) tea.Cmd {
	blinkCmd := fleetPage.activateAddInstanceField()
	inputCmd := fleetPage.updateActiveAddInstanceField(msg)
	return tea.Batch(blinkCmd, inputCmd)
}

// addInstanceRowEnabled reports whether a given row is selectable in the
// current dialog mode. Branch and deploy are locked while editing because
// they describe how the workspace was originally provisioned and cannot
// be retroactively changed without recreating the instance.
func (fleetPage *fleetPage) addInstanceRowEnabled(row int) bool {
	if !fleetPage.dialogEditing {
		return true
	}
	return row == addInstanceRowName || row == addInstanceRowColor
}

// nextAddInstanceRow advances the focused row forward, skipping any rows
// that are disabled in the current dialog mode.
func (fleetPage *fleetPage) nextAddInstanceRow(current int) int {
	for i := 1; i <= addInstanceRowCount; i++ {
		candidate := (current + i) % addInstanceRowCount
		if fleetPage.addInstanceRowEnabled(candidate) {
			return candidate
		}
	}
	return current
}

// prevAddInstanceRow advances the focused row backward, skipping any rows
// that are disabled in the current dialog mode.
func (fleetPage *fleetPage) prevAddInstanceRow(current int) int {
	for i := 1; i <= addInstanceRowCount; i++ {
		candidate := (current - i + addInstanceRowCount) % addInstanceRowCount
		if fleetPage.addInstanceRowEnabled(candidate) {
			return candidate
		}
	}
	return current
}

// saveInstanceEdits commits display-name and color edits to the selected
// instance and closes the dialog. The underlying Name is immutable; the
// name input writes to DisplayName instead.
func (fleetPage *fleetPage) saveInstanceEdits(m *model) tea.Cmd {
	f, ok := m.st.Fleets[fleetPage.dialogFleet]
	if !ok {
		fleetPage.mode = viewNormal
		fleetPage.dialogEditing = false
		fleetPage.blurDialogFields()
		m.message = fmt.Sprintf("Fleet %s not found", fleetPage.dialogFleet)
		return nil
	}
	instance, err := f.GetInstance(fleetPage.dialogInst)
	if err != nil {
		fleetPage.mode = viewNormal
		fleetPage.dialogEditing = false
		fleetPage.blurDialogFields()
		m.message = fmt.Sprintf("Instance %s/%s not found", fleetPage.dialogFleet, fleetPage.dialogInst)
		return nil
	}

	displayName := strings.TrimSpace(fleetPage.textInput.Value())
	if displayName == "" {
		m.message = "Name cannot be empty"
		return nil
	}

	color := fleetPage.dialogColor
	if color == instanceColorWhite {
		color = ""
	}
	instance.DisplayName = displayName
	instance.Color = color
	_ = setInstanceMetadataRemote(fleetPage.dialogFleet, fleetPage.dialogInst, &displayName, &color, nil)

	fleetPage.buildRows(m)
	fleetPage.mode = viewNormal
	fleetPage.dialogEditing = false
	fleetPage.blurDialogFields()
	m.message = fmt.Sprintf("Updated %s/%s", fleetPage.dialogFleet, fleetPage.dialogInst)
	return nil
}

// ===========================================
// Tag Instance Dialog
// ===========================================

// updateTagInstance handles the tag-instance dialog.
func (fleetPage *fleetPage) updateTagInstance(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dialogFieldActive {
			switch msg.String() {
			case "enter":
				return fleetPage.saveTagInstance(m)
			case "esc":
				fleetPage.deactivateTextInput()
				return nil
			case "ctrl+c":
				return fleetPage.cancelTextDialog(m)
			}
			var cmd tea.Cmd
			fleetPage.textInput, cmd = fleetPage.textInput.Update(msg)
			return cmd
		}

		switch msg.String() {
		case "enter":
			return fleetPage.activateTextInput()
		case " ":
			return fleetPage.activateTextInput()
		case "esc", "q", "Q", "ctrl+c":
			return fleetPage.cancelTextDialog(m)
		}
		if isDialogTextKey(msg) {
			return fleetPage.activateTextInputWithMsg(msg)
		}
	}

	return nil
}

func (fleetPage *fleetPage) saveTagInstance(m *model) tea.Cmd {
	tag := strings.TrimSpace(fleetPage.textInput.Value())

	f, ok := m.st.Fleets[fleetPage.dialogFleet]
	if ok {
		if instance, err := f.GetInstance(fleetPage.dialogInst); err == nil {
			instance.Tag = tag
			_ = setInstanceMetadataRemote(fleetPage.dialogFleet, fleetPage.dialogInst, nil, nil, &tag)
		}
	}

	fleetPage.mode = viewNormal
	fleetPage.blurDialogFields()
	if tag == "" {
		m.message = fmt.Sprintf("Cleared tag for %s/%s", fleetPage.dialogFleet, fleetPage.dialogInst)
	} else {
		m.message = fmt.Sprintf("Tagged %s/%s: %s", fleetPage.dialogFleet, fleetPage.dialogInst, tag)
	}
	return nil
}

func (fleetPage *fleetPage) cancelTextDialog(m *model) tea.Cmd {
	fleetPage.mode = viewNormal
	fleetPage.blurDialogFields()
	m.message = "Cancelled"
	return nil
}

// ===========================================
// Add Fleet Dialog
// ===========================================

// updateAddFleet handles the add-fleet dialog.
//
// Pressing enter does not immediately persist the fleet — instead, it
// kicks off an asynchronous inspection (clone + .devcontainer lookup)
// and switches to viewAddFleetInspecting so the user sees a spinner
// while the network work runs. The inspect result is delivered via
// devcontainerInspectedMsg and resumed in handleDevcontainerInspected.
func (fleetPage *fleetPage) updateAddFleet(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dialogFieldActive {
			switch msg.String() {
			case "enter":
				return fleetPage.saveAddFleet(m)
			case "esc":
				fleetPage.deactivateTextInput()
				return nil
			case "ctrl+c":
				return fleetPage.cancelTextDialog(m)
			}
			var cmd tea.Cmd
			fleetPage.textInput, cmd = fleetPage.textInput.Update(msg)
			return cmd
		}

		switch msg.String() {
		case "enter":
			return fleetPage.activateTextInput()
		case " ":
			return fleetPage.activateTextInput()
		case "esc", "q", "Q", "ctrl+c":
			return fleetPage.cancelTextDialog(m)
		}
		if isDialogTextKey(msg) {
			return fleetPage.activateTextInputWithMsg(msg)
		}
	}

	return nil
}

// saveAddFleet validates the URL and kicks off the asynchronous
// devcontainer inspection. The fleet is NOT persisted here — that
// happens later in handleDevcontainerInspected (devcontainer present)
// or in updateAddFleetNoDevcontainer's Setup branch (devcontainer
// missing but user opted into the agent flow). Aborting either dialog
// after this point therefore leaves no trace in state.
func (fleetPage *fleetPage) saveAddFleet(m *model) tea.Cmd {
	repoURL := strings.TrimSpace(fleetPage.textInput.Value())
	if repoURL == "" {
		m.message = "URL cannot be empty"
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}
	fleetName := fleet.FleetNameFromRemote(repoURL)
	if fleetName == "" {
		m.message = "Could not derive fleet name from URL"
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}

	fleetPage.dialogPendingRepoURL = repoURL
	fleetPage.dialogPendingFleetName = fleetName
	fleetPage.mode = viewAddFleetInspecting
	fleetPage.blurDialogFields()
	m.message = fmt.Sprintf("Inspecting %s...", repoURL)
	return inspectDevcontainerCmd(fleetName, repoURL)
}

// ===========================================
// Devcontainer Inspection (new fleet)
// ===========================================

// devcontainerInspectedMsg is delivered when the asynchronous repo
// clone + devcontainer.json lookup completes. The fleetName is echoed
// back so a stale result (the user dismissed the dialog before the
// clone finished) can be discarded.
type devcontainerInspectedMsg struct {
	fleetName       string
	hasDevcontainer bool
	err             error
}

// inspectDevcontainerCmd runs a shallow clone and a devcontainer-check
// in a background goroutine. The Repo handle is closed before the
// message is returned so the temp clone never outlives the command.
//
// A clone failure surfaces with err set so the caller can report it
// rather than blindly assuming the repo lacks a devcontainer — an
// unreachable URL is a different problem than a configured-but-missing
// devcontainer, and the user almost certainly wants to fix the URL
// before being offered a setup workflow.
func inspectDevcontainerCmd(fleetName, remoteURL string) tea.Cmd {
	return func() tea.Msg {
		repo, err := inspector.Open(remoteURL, "")
		if err != nil {
			return devcontainerInspectedMsg{fleetName: fleetName, err: err}
		}
		defer repo.Close()
		present, err := devcontainercheck.Present(repo)
		return devcontainerInspectedMsg{
			fleetName:       fleetName,
			hasDevcontainer: present,
			err:             err,
		}
	}
}

// handleDevcontainerInspected resumes the new-fleet flow once the
// asynchronous inspection has completed. There are three branches:
//
//  1. clone failed → surface the error, drop back to the URL input so
//     the user can correct it. The fleet is not persisted.
//  2. devcontainer present → persist the fleet immediately and dismiss.
//  3. devcontainer missing → switch to the no-devcontainer dialog so
//     the user can choose to abort or launch the setup agent.
//
// Stale results from a dialog the user has already abandoned are
// dropped silently.
func (fleetPage *fleetPage) handleDevcontainerInspected(m *model, msg devcontainerInspectedMsg) tea.Cmd {
	if fleetPage.mode != viewAddFleetInspecting || fleetPage.dialogPendingFleetName != msg.fleetName {
		return nil
	}

	if msg.err != nil {
		fleetPage.mode = viewAddFleet
		fleetPage.textInput.Focus()
		m.message = fmt.Sprintf("Could not inspect repo: %v", msg.err)
		return fleetPage.textInput.Cursor.BlinkCmd()
	}

	if msg.hasDevcontainer {
		fleetPage.addPendingFleet(m)
		m.message = fmt.Sprintf("Added fleet %s", fleetPage.dialogPendingFleetName)
		fleetPage.clearPendingFleet()
		fleetPage.mode = viewNormal
		return nil
	}

	fleetPage.mode = viewAddFleetNoDevcontainer
	return nil
}

// addPendingFleet creates the fleet record for whichever URL is
// currently pending and rebuilds the row list. Used by both the
// "devcontainer present → just add it" success path and the
// "user picked Setup → optimistically add then hand off to agent"
// branch.
func (fleetPage *fleetPage) addPendingFleet(m *model) {
	m.st.GetOrCreateFleet(fleetPage.dialogPendingFleetName, fleetPage.dialogPendingRepoURL)
	_ = createFleetRemote(fleetPage.dialogPendingFleetName, fleetPage.dialogPendingRepoURL)
	fleetPage.buildRows(m)
}

// clearPendingFleet wipes the per-dialog scratch fields once the
// inspect/setup workflow finishes (success, abort, or error). The
// values are not load-bearing after the dialog closes; resetting them
// keeps a future open-this-dialog-again from seeing stale data.
func (fleetPage *fleetPage) clearPendingFleet() {
	fleetPage.dialogPendingRepoURL = ""
	fleetPage.dialogPendingFleetName = ""
}

// ===========================================
// No-Devcontainer Dialog
// ===========================================

// updateAddFleetInspecting handles input while the
// "Inspecting <repo>..." spinner is on screen. The user can press esc /
// ctrl+c to bail out of the new-fleet flow without waiting for the
// clone to finish (the goroutine will still complete and the result
// will be dropped by the stale-mode guard in
// handleDevcontainerInspected).
func (fleetPage *fleetPage) updateAddFleetInspecting(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch keyMsg.String() {
	case "esc", "q", "Q", "ctrl+c":
		fleetPage.mode = viewNormal
		fleetPage.clearPendingFleet()
		m.message = "Cancelled"
	}
	return nil
}

// updateAddFleetNoDevcontainer handles the dialog shown when the
// inspected repo has no devcontainer.json. Two paths:
//
//   - Abort (default; esc / n / a / enter): drop the pending fleet
//     and return to the fleet list without persisting anything.
//   - Setup (s): persist the fleet optimistically (so the user can see
//     it in the list while they work) and hand off to the local
//     coding agent with a devcontainer-authoring prompt. The agent's
//     stdio takes over the terminal; when it exits (ctrl+c / ctrl+d)
//     bubbletea repaints and we are back in the fleet list.
func (fleetPage *fleetPage) updateAddFleetNoDevcontainer(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch keyMsg.String() {
	case "s", "S":
		repoURL := fleetPage.dialogPendingRepoURL
		fleetName := fleetPage.dialogPendingFleetName

		cmd, err := devcontainersetup.Command(repoURL)
		if err != nil {
			fleetPage.mode = viewNormal
			fleetPage.clearPendingFleet()
			m.message = fmt.Sprintf("No coding agent available: %v", err)
			return nil
		}

		// Add the fleet immediately, before launching the agent. The
		// issue spec is explicit: assume the user follows through. If
		// they bail mid-setup the fleet still appears in the list so
		// they can return to it (or delete it) later.
		fleetPage.addPendingFleet(m)
		m.message = fmt.Sprintf("Added fleet %s — launching setup agent...", fleetName)
		fleetPage.clearPendingFleet()
		fleetPage.mode = viewNormal

		return tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })

	case "a", "A", "n", "N", "q", "Q", "esc", "ctrl+c", "enter":
		fleetPage.mode = viewNormal
		fleetPage.clearPendingFleet()
		m.message = "Cancelled — fleet not added"
		return nil
	}
	return nil
}

// ===========================================
// Edit Fleet Dialog
// ===========================================

// editFleetRow identifies a focusable row in the edit-fleet dialog.
const (
	editFleetRowClaude = iota
	editFleetRowCodex
	editFleetRowGh
	editFleetRowHomeDir
	editFleetRowPreferFleetLaunch
	editFleetRowCount
)

// homedirDetectedMsg is delivered when an asynchronous home-directory
// detection cmd completes. The fleetName lets the receiver discard
// stale results when the user has moved on to a different fleet.
type homedirDetectedMsg struct {
	fleetName string
	homeDir   string
	err       error
}

// detectHomedirCmd opens an inspector handle for the fleet's remote
// and runs the home-dir check against it in a background goroutine.
// The handle is closed before the message is returned so the temp
// clone never outlives the command.
//
// Errors are surfaced as part of homedirDetectedMsg; the caller
// treats them the same as a successful empty result (spinner stops,
// nothing populated) because failure is expected (no devcontainer.json,
// missing docker, network blocked, …) and the user can always type a
// path manually.
func detectHomedirCmd(fleetName, remoteURL, branch string) tea.Cmd {
	return func() tea.Msg {
		repo, err := inspector.Open(remoteURL, branch)
		if err != nil {
			return homedirDetectedMsg{fleetName: fleetName, err: err}
		}
		defer repo.Close()
		homeDir, err := homedircheck.Detect(repo)
		return homedirDetectedMsg{fleetName: fleetName, homeDir: homeDir, err: err}
	}
}

// openEditFleetDialog opens the edit-fleet dialog for the fleet at the
// cursor. The dialog edits FleetSettings — the Claude Code / Codex
// shared-mount toggles plus the container-side HomeDir those mounts
// resolve under. Settings declare the user's intent; supported backends
// honor them at instance-creation time, others silently skip.
//
// When the fleet already has a mount enabled but no HomeDir, this
// function kicks off the detector immediately so the user does not
// have to re-toggle to recover an empty value.
func (fleetPage *fleetPage) openEditFleetDialog(m *model) tea.Cmd {
	r := fleetPage.currentRow()
	if r == nil || r.kind != rowFleetHeader {
		m.message = "Select a fleet to edit"
		return nil
	}
	f, ok := m.st.Fleets[r.fleetName]
	if !ok {
		m.message = fmt.Sprintf("Fleet %s not found", r.fleetName)
		return nil
	}

	fleetPage.mode = viewEditFleet
	fleetPage.dialogFleet = f.Name
	fleetPage.dialogClaudeMount = f.Settings.ClaudeCodeMount
	fleetPage.dialogCodexMount = f.Settings.CodexMount
	fleetPage.dialogGhMount = f.Settings.GhMount
	fleetPage.dialogPreferFleetLaunch = f.Settings.PreferFleetLaunchEnabled()
	fleetPage.dialogRow = editFleetRowClaude
	fleetPage.dialogDetecting = false
	fleetPage.dialogFieldActive = false

	fleetPage.homedirInput.SetValue(f.Settings.HomeDir)
	fleetPage.homedirInput.Blur()

	if fleetPage.shouldKickHomedirDetect(f) {
		return fleetPage.startHomedirDetect(f)
	}
	return nil
}

// updateEditFleet handles the edit-fleet dialog: arrow-key navigation
// between rows, space/x toggling the mount checkboxes, character keys
// editing the home-dir text input, enter committing, esc cancelling.
func (fleetPage *fleetPage) updateEditFleet(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}

	if fleetPage.dialogFieldActive {
		switch keyMsg.String() {
		case "enter":
			return fleetPage.saveFleetEdits(m)
		case "esc":
			fleetPage.dialogFieldActive = false
			fleetPage.syncEditFleetFocus()
			return nil
		case "ctrl+c":
			fleetPage.mode = viewNormal
			fleetPage.blurDialogFields()
			m.message = "Cancelled"
			return nil
		}
		if fleetPage.dialogRow == editFleetRowHomeDir {
			var cmd tea.Cmd
			fleetPage.homedirInput, cmd = fleetPage.homedirInput.Update(msg)
			return cmd
		}
	}

	switch keyMsg.String() {
	case "enter":
		if fleetPage.dialogRow == editFleetRowHomeDir {
			fleetPage.dialogFieldActive = true
			fleetPage.syncEditFleetFocus()
			return fleetPage.homedirInput.Cursor.BlinkCmd()
		}
		return fleetPage.saveFleetEdits(m)

	case "up", "k":
		fleetPage.dialogFieldActive = false
		fleetPage.dialogRow = (fleetPage.dialogRow - 1 + editFleetRowCount) % editFleetRowCount
		fleetPage.syncEditFleetFocus()
		return nil

	case "down", "j", "tab":
		fleetPage.dialogFieldActive = false
		fleetPage.dialogRow = (fleetPage.dialogRow + 1) % editFleetRowCount
		fleetPage.syncEditFleetFocus()
		return nil

	case "esc", "q", "Q", "ctrl+c":
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		m.message = "Cancelled"
		return nil
	}

	// Toggle / character input is row-specific.
	switch fleetPage.dialogRow {
	case editFleetRowClaude, editFleetRowCodex, editFleetRowGh, editFleetRowPreferFleetLaunch:
		switch keyMsg.String() {
		case " ", "left", "right", "h", "l", "x":
			return fleetPage.toggleEditFleetRow(m)
		}
		return nil
	case editFleetRowHomeDir:
		switch keyMsg.String() {
		case " ":
			fleetPage.dialogFieldActive = true
			fleetPage.syncEditFleetFocus()
			return fleetPage.homedirInput.Cursor.BlinkCmd()
		}
		if isDialogTextKey(keyMsg) {
			fleetPage.dialogFieldActive = true
			fleetPage.syncEditFleetFocus()
			blinkCmd := fleetPage.homedirInput.Cursor.BlinkCmd()
			var inputCmd tea.Cmd
			fleetPage.homedirInput, inputCmd = fleetPage.homedirInput.Update(msg)
			return tea.Batch(blinkCmd, inputCmd)
		}
	}
	return nil
}

// toggleEditFleetRow flips the boolean for the currently focused
// checkbox row. When a mount is being turned on it may also kick off
// auto-detection of the container's home directory so the user does
// not have to type it themselves.
func (fleetPage *fleetPage) toggleEditFleetRow(m *model) tea.Cmd {
	turnedOn := false
	switch fleetPage.dialogRow {
	case editFleetRowClaude:
		fleetPage.dialogClaudeMount = !fleetPage.dialogClaudeMount
		turnedOn = fleetPage.dialogClaudeMount
	case editFleetRowCodex:
		fleetPage.dialogCodexMount = !fleetPage.dialogCodexMount
		turnedOn = fleetPage.dialogCodexMount
	case editFleetRowGh:
		fleetPage.dialogGhMount = !fleetPage.dialogGhMount
		turnedOn = fleetPage.dialogGhMount
	case editFleetRowPreferFleetLaunch:
		// Not a mount, so no home-dir detection to kick off — just flip it.
		fleetPage.dialogPreferFleetLaunch = !fleetPage.dialogPreferFleetLaunch
		return nil
	}
	if !turnedOn {
		return nil
	}
	f, ok := m.st.Fleets[fleetPage.dialogFleet]
	if !ok {
		return nil
	}
	if fleetPage.shouldKickHomedirDetect(f) {
		return fleetPage.startHomedirDetect(f)
	}
	return nil
}

// shouldKickHomedirDetect reports whether conditions are right to
// trigger an auto-detection: at least one mount is enabled in the
// current dialog state, the home-dir text input is empty, no detection
// is already in flight, and the fleet has a remote URL we can clone.
func (fleetPage *fleetPage) shouldKickHomedirDetect(f *fleet.Fleet) bool {
	if fleetPage.dialogDetecting {
		return false
	}
	if strings.TrimSpace(fleetPage.homedirInput.Value()) != "" {
		return false
	}
	if !fleetPage.dialogClaudeMount && !fleetPage.dialogCodexMount && !fleetPage.dialogGhMount {
		return false
	}
	return strings.TrimSpace(f.Remote) != ""
}

// startHomedirDetect marks detection as in flight and returns the cmd
// that performs the actual clone+inspect work in the background.
func (fleetPage *fleetPage) startHomedirDetect(f *fleet.Fleet) tea.Cmd {
	fleetPage.dialogDetecting = true
	return detectHomedirCmd(f.Name, f.Remote, "")
}

// handleHomedirDetected applies the result of an auto-detection. The
// guard checks ensure stale results — from a fleet the user has since
// closed, or arriving after the user has already typed a value —
// never overwrite live state.
func (fleetPage *fleetPage) handleHomedirDetected(msg homedirDetectedMsg) {
	// Always clear the in-flight flag for *this* fleet so the spinner
	// stops, even when the result is not applied.
	if fleetPage.dialogFleet == msg.fleetName {
		fleetPage.dialogDetecting = false
	}
	if msg.err != nil || msg.homeDir == "" {
		return
	}
	if fleetPage.mode != viewEditFleet || fleetPage.dialogFleet != msg.fleetName {
		return
	}
	if strings.TrimSpace(fleetPage.homedirInput.Value()) != "" {
		return
	}
	fleetPage.homedirInput.SetValue(msg.homeDir)
}

// syncEditFleetFocus moves the cursor blink to the home-dir input only
// when that row is the currently selected row. Other rows have no
// editable text so the input must blur to avoid a stray cursor.
func (fleetPage *fleetPage) syncEditFleetFocus() {
	if fleetPage.dialogFieldActive && fleetPage.dialogRow == editFleetRowHomeDir {
		fleetPage.homedirInput.Focus()
	} else {
		fleetPage.homedirInput.Blur()
	}
}

// saveFleetEdits commits the dialog's mount toggles + home-dir to the
// fleet's persisted settings and closes the dialog. Existing instances
// are not retroactively re-mounted; the new settings apply to the next
// instance provisioned on a supporting backend.
func (fleetPage *fleetPage) saveFleetEdits(m *model) tea.Cmd {
	f, ok := m.st.Fleets[fleetPage.dialogFleet]
	if !ok {
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		m.message = fmt.Sprintf("Fleet %s not found", fleetPage.dialogFleet)
		return nil
	}
	f.Settings.ClaudeCodeMount = fleetPage.dialogClaudeMount
	f.Settings.CodexMount = fleetPage.dialogCodexMount
	f.Settings.GhMount = fleetPage.dialogGhMount
	preferFleetLaunch := fleetPage.dialogPreferFleetLaunch
	f.Settings.PreferFleetLaunch = &preferFleetLaunch
	f.Settings.HomeDir = strings.TrimSpace(fleetPage.homedirInput.Value())
	_ = setFleetSettingsRemote(fleetPage.dialogFleet, f.Settings)

	fleetPage.mode = viewNormal
	fleetPage.blurDialogFields()
	m.message = fmt.Sprintf("Updated %s settings", f.Name)
	return nil
}

// ===========================================
// Port Forward Dialog
// ===========================================

// updatePortForward handles the port forward management dialog.
func (fleetPage *fleetPage) updatePortForward(m *model, msg tea.Msg) tea.Cmd {
	key := fleetPage.dialogFleet + "/" + fleetPage.dialogInst

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dialogFieldActive {
			switch msg.String() {
			case "enter":
				return fleetPage.addPortForward(m, key)
			case "esc":
				fleetPage.deactivateTextInput()
				return nil
			case "ctrl+c":
				return fleetPage.closePortForward(m, key)
			}
			var cmd tea.Cmd
			fleetPage.textInput, cmd = fleetPage.textInput.Update(msg)
			return cmd
		}

		switch msg.String() {
		case "enter":
			return fleetPage.activateTextInput()
		case " ":
			return fleetPage.activateTextInput()

		case "d":
			fwds := m.portForwards.List(key)
			if len(fwds) > 0 && fleetPage.pfCursor < len(fwds) {
				fwd := fwds[fleetPage.pfCursor]
				_ = m.portForwards.Remove(key, fwd.LocalPort)
				m.message = fmt.Sprintf("Removed forward %s", fwd.Label())
				if fleetPage.pfCursor >= len(m.portForwards.List(key)) {
					fleetPage.pfCursor = max(0, fleetPage.pfCursor-1)
				}
				return nil
			}

		case "up", "k":
			if fleetPage.pfCursor > 0 {
				fleetPage.pfCursor--
			}
			return nil

		case "down", "j":
			fwds := m.portForwards.List(key)
			if fleetPage.pfCursor < len(fwds)-1 {
				fleetPage.pfCursor++
			}
			return nil

		case "esc", "q", "Q", "ctrl+c":
			return fleetPage.closePortForward(m, key)
		}
		if isDialogTextKey(msg) {
			return fleetPage.activateTextInputWithMsg(msg)
		}
	}

	return nil
}

func (fleetPage *fleetPage) addPortForward(m *model, key string) tea.Cmd {
	mappingInput := strings.TrimSpace(fleetPage.textInput.Value())
	if mappingInput == "" {
		return nil
	}
	local, remote, err := parsePortMapping(mappingInput)
	if err != nil {
		m.message = err.Error()
		return nil
	}

	instanceBackend := m.instanceBackend(&fleet.Instance{Backend: fleetPage.instanceBackendType(m)})
	if err := m.portForwards.Add(key, local, remote, instanceBackend.PortForwardCommand, fleetPage.pfContainerID, instanceBackend.ResolveHostname); err != nil {
		m.message = err.Error()
		return nil
	}

	fleetPage.textInput.SetValue("")
	m.message = fmt.Sprintf("Forwarding localhost:%d -> %s:%d", local, fleetPage.dialogInst, remote)
	return nil
}

func (fleetPage *fleetPage) closePortForward(m *model, key string) tea.Cmd {
	fleetPage.mode = viewNormal
	fleetPage.blurDialogFields()
	fwds := m.portForwards.List(key)
	if len(fwds) > 0 {
		m.message = fmt.Sprintf("%d port forward(s) active on %s", len(fwds), fleetPage.dialogInst)
	} else {
		m.message = ""
	}
	return nil
}

// parsePortMapping splits a "local:remote" string into two port numbers.
func parsePortMapping(raw string) (int, int, error) {
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected local:remote (e.g. 8080:80)")
	}
	local, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || local < 1 || local > 65535 {
		return 0, 0, fmt.Errorf("invalid local port %q", parts[0])
	}
	remote, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || remote < 1 || remote > 65535 {
		return 0, 0, fmt.Errorf("invalid remote port %q", parts[1])
	}
	return local, remote, nil
}

// ===========================================
// Codespaces Auth Scope Dialog
// ===========================================

// updateCodespacesAuth handles the dialog shown when gh is missing
// the "codespace" OAuth scope.
func (fleetPage *fleetPage) updateCodespacesAuth(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			fleetPage.mode = viewNormal
			m.message = "Launching GitHub auth..."
			return tea.ExecProcess(
				exec.Command("gh", "auth", "login", "-h", "github.com", "-s", "codespace"),
				func(err error) tea.Msg {
					if err != nil {
						return execDoneMsg{fmt.Errorf("gh auth login: %w", err)}
					}
					return execDoneMsg{}
				},
			)
		case "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			m.message = "Auth cancelled — codespace creation requires the codespace scope"
		}
	}
	return nil
}

// updateCodespacesMachine handles the dialog shown when gh needs a
// machine type but none is configured.
func (fleetPage *fleetPage) updateCodespacesMachine(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			fleetPage.mode = viewNormal
			m.message = "Set the Machine field, then retry instance creation"
			return m.ChangeRoute(routeSettings)
		case "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			m.message = ""
		}
	}
	return nil
}

// updateCodespacesLimit handles the dialog shown when the user has
// hit the maximum codespace count.
func (fleetPage *fleetPage) updateCodespacesLimit(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter", "esc", "q", "Q", "ctrl+c":
			fleetPage.mode = viewNormal
			m.message = ""
		}
	}
	return nil
}

// ===========================================
// Port Forward Helpers
// ===========================================

// instanceBackendType returns the backend type for the instance currently
// being managed in the port forward dialog.
func (fleetPage *fleetPage) instanceBackendType(m *model) fleet.BackendType {
	if f, ok := m.st.Fleets[fleetPage.dialogFleet]; ok {
		if instance, err := f.GetInstance(fleetPage.dialogInst); err == nil {
			return instance.Backend
		}
	}
	return fleet.BackendDevcontainer
}

// ===========================================
// Session Dialogs
// ===========================================

// updateCreateSession handles the dialog for creating a new tmux session.
func (fleetPage *fleetPage) updateCreateSession(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dialogFieldActive {
			switch msg.String() {
			case "enter":
				return fleetPage.saveCreateSession(m)
			case "esc":
				fleetPage.deactivateTextInput()
				return nil
			case "ctrl+c":
				return fleetPage.cancelTextDialog(m)
			}
			var cmd tea.Cmd
			fleetPage.textInput, cmd = fleetPage.textInput.Update(msg)
			return cmd
		}

		switch msg.String() {
		case "enter":
			return fleetPage.activateTextInput()
		case " ":
			return fleetPage.activateTextInput()
		case "esc", "q", "Q", "ctrl+c":
			return fleetPage.cancelTextDialog(m)
		}
		if isDialogTextKey(msg) {
			return fleetPage.activateTextInputWithMsg(msg)
		}
	}

	return nil
}

func (fleetPage *fleetPage) saveCreateSession(m *model) tea.Cmd {
	name := strings.TrimSpace(fleetPage.textInput.Value())
	ref := InstanceRef{Fleet: fleetPage.dialogFleet, Instance: fleetPage.dialogInst}
	if name == "" {
		name = nextSessionName(m.sessionStore.Sessions(ref))
	}
	name = SanitizeSessionName(name)

	f, ok := m.st.Fleets[fleetPage.dialogFleet]
	if !ok {
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}
	instance, err := f.GetInstance(fleetPage.dialogInst)
	if err != nil {
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}

	sanitized := SanitizeSessionName(instance.Name)
	fullName := groupSessionName(sanitized, name)

	fleetPage.mode = viewNormal
	fleetPage.blurDialogFields()
	m.message = fmt.Sprintf("Creating session %s...", name)
	instanceBackend := m.instanceBackend(instance)
	return createSessionCmd(instanceBackend, instance.WorkspaceDir, ref, fullName)
}

// updateCloneInstance handles the single-text-input dialog that asks
// the user for a destination instance name when cloning. dialogFleet
// and dialogInst hold the source instance's identifiers.
func (fleetPage *fleetPage) updateCloneInstance(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dialogFieldActive {
			switch msg.String() {
			case "enter":
				return fleetPage.saveCloneInstance(m)
			case "esc":
				fleetPage.deactivateTextInput()
				return nil
			case "ctrl+c":
				return fleetPage.cancelTextDialog(m)
			}
			var cmd tea.Cmd
			fleetPage.textInput, cmd = fleetPage.textInput.Update(msg)
			return cmd
		}

		switch msg.String() {
		case "enter", " ":
			return fleetPage.activateTextInput()
		case "esc", "q", "Q", "ctrl+c":
			return fleetPage.cancelTextDialog(m)
		}
		if isDialogTextKey(msg) {
			return fleetPage.activateTextInputWithMsg(msg)
		}
	}

	return nil
}

// saveCloneInstance validates the destination name and dispatches a server-side
// CloneInstance job (which pre-creates the StatusCloning record and copies the
// source's settings); the TUI tracks progress via reload() + pollCreating.
func (fleetPage *fleetPage) saveCloneInstance(m *model) tea.Cmd {
	destName := strings.TrimSpace(fleetPage.textInput.Value())
	if destName == "" {
		m.message = "Name cannot be empty"
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}

	fleetName := fleetPage.dialogFleet
	srcName := fleetPage.dialogInst

	f, ok := m.st.Fleets[fleetName]
	if !ok {
		m.message = fmt.Sprintf("Fleet %s not found", fleetName)
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}
	if _, err := f.GetInstance(srcName); err != nil {
		m.message = fmt.Sprintf("Source instance %s/%s not found", fleetName, srcName)
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}
	if _, err := f.GetInstance(destName); err == nil {
		m.message = fmt.Sprintf("Instance %s/%s already exists", fleetName, destName)
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}

	// The destination record is pre-created server-side by the CloneInstance job
	// (which copies the source's config/backend/tag/color/branch); no client
	// write. instanceSpawnedMsg reload()s it into view.
	key := fleetName + "/" + destName
	m.creating[key] = true
	fleetPage.mode = viewNormal
	fleetPage.blurDialogFields()
	m.message = fmt.Sprintf("Cloning %s/%s -> %s...", fleetName, srcName, destName)

	return cloneInstanceCmd(fleetName, srcName, destName)
}

// updateRenameSession handles the dialog for renaming a tmux session.
func (fleetPage *fleetPage) updateRenameSession(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dialogFieldActive {
			switch msg.String() {
			case "enter":
				return fleetPage.saveRenameSession(m)
			case "esc":
				fleetPage.deactivateTextInput()
				return nil
			case "ctrl+c":
				return fleetPage.cancelTextDialog(m)
			}
			var cmd tea.Cmd
			fleetPage.textInput, cmd = fleetPage.textInput.Update(msg)
			return cmd
		}

		switch msg.String() {
		case "enter":
			return fleetPage.activateTextInput()
		case " ":
			return fleetPage.activateTextInput()
		case "esc", "q", "Q", "ctrl+c":
			return fleetPage.cancelTextDialog(m)
		}
		if isDialogTextKey(msg) {
			return fleetPage.activateTextInputWithMsg(msg)
		}
	}

	return nil
}

func (fleetPage *fleetPage) saveRenameSession(m *model) tea.Cmd {
	newName := strings.TrimSpace(fleetPage.textInput.Value())
	if newName == "" {
		m.message = "Name cannot be empty"
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}
	newName = SanitizeSessionName(newName)

	ref := InstanceRef{Fleet: fleetPage.dialogFleet, Instance: fleetPage.dialogInst}

	f, ok := m.st.Fleets[fleetPage.dialogFleet]
	if !ok {
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}
	instance, err := f.GetInstance(fleetPage.dialogInst)
	if err != nil {
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}

	sanitized := SanitizeSessionName(instance.Name)
	oldName := fleetPage.dialogSession
	oldGID, isGrouped := parseGroupID(sanitized, oldName)

	fleetPage.mode = viewNormal
	fleetPage.blurDialogFields()
	instanceBackend := m.instanceBackend(instance)

	if isGrouped {
		return renameGroupCmd(instanceBackend, instance.WorkspaceDir, ref, sanitized, oldGID, newName)
	}
	return renameSessionCmd(instanceBackend, instance.WorkspaceDir, ref, oldName, newName)
}
