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
			instanceKey := fleetName + "/" + instanceName

			// Switch the dialog into "in-flight" mode; the renderer
			// will draw the spinner. The cmd does the kill + relaunch
			// in the background and the resulting browserProxyMsg
			// clears the flag and the mode.
			fleetPage.dialogBrowserSwitching = true
			m.message = ""
			return switchBrowserCmd(m.portForwards, instance, instanceKey, dataDir, f.Settings.PreferFleetLaunchEnabled(), "")
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
			sanitized := SanitizeSessionName(instance.Name)
			if fleetPage.dialogGroupID != "" && isGroupedSession(sanitized, fleetPage.dialogSession) {
				return deleteGroupSessionsCmd(ref, sanitized, fleetPage.dialogGroupID)
			}
			return deleteSessionCmd(ref, fleetPage.dialogSession)

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

		return execProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err} })

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
	editFleetRowCaching  // collapsible section header
	editFleetRowBuildkit // child of Caching; only navigable when expanded
	editFleetRowCount
)

// visibleEditFleetRows returns the edit-fleet dialog's navigable rows in display
// order. The Buildkit row only appears while the Caching section is expanded.
func (fleetPage *fleetPage) visibleEditFleetRows() []int {
	rows := []int{
		editFleetRowClaude,
		editFleetRowCodex,
		editFleetRowGh,
		editFleetRowHomeDir,
		editFleetRowPreferFleetLaunch,
		editFleetRowCaching,
	}
	if fleetPage.dialogCachingExpanded {
		rows = append(rows, editFleetRowBuildkit)
	}
	return rows
}

// moveEditFleetRow moves the dialog cursor by delta within the visible rows,
// wrapping, and resets the per-row sub-state (button focus / delete confirm).
func (fleetPage *fleetPage) moveEditFleetRow(delta int) {
	rows := fleetPage.visibleEditFleetRows()
	idx, found := 0, false
	for i, r := range rows {
		if r == fleetPage.dialogRow {
			idx, found = i, true
			break
		}
	}
	if found {
		fleetPage.dialogRow = rows[(idx+delta+len(rows))%len(rows)]
	} else {
		// The current row is no longer visible (e.g. its section collapsed under
		// the cursor) — recover by landing on the first visible row.
		fleetPage.dialogRow = rows[0]
	}
	fleetPage.dialogBuildkitButtonFocused = false
	fleetPage.dialogDeleteCacheConfirm = false
	fleetPage.syncEditFleetFocus()
}

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
	fleetPage.dialogBuildkitServer = f.Settings.BuildkitServer
	fleetPage.dialogPreferFleetLaunch = f.Settings.PreferFleetLaunchEnabled()
	fleetPage.dialogPreferFleetLaunchSet = f.Settings.PreferFleetLaunchSet()
	fleetPage.dialogRow = editFleetRowClaude
	fleetPage.dialogDetecting = false
	fleetPage.dialogFieldActive = false
	fleetPage.dialogCachingExpanded = false
	fleetPage.dialogBuildkitButtonFocused = false
	fleetPage.dialogDeleteCacheConfirm = false
	fleetPage.dialogDeletingCache = false

	fleetPage.homedirInput.SetValue(f.Settings.HomeDir)
	fleetPage.homedirInput.Blur()

	if fleetPage.shouldKickHomedirDetect(f) {
		return fleetPage.startHomedirDetect(f)
	}
	return nil
}

// updateEditFleet handles the edit-fleet dialog. The dialog is INSTANT-SAVE
// (like the settings page): every toggle and every committed home-dir edit
// persists immediately, so there is no explicit "save" key — esc/q just closes,
// and a per-change RPC failure is reverted in place.
func (fleetPage *fleetPage) updateEditFleet(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}

	// Home-dir text-editing sub-mode.
	if fleetPage.dialogFieldActive {
		switch keyMsg.String() {
		case "enter":
			// Commit the typed value (instant-save) and leave editing.
			cmd := fleetPage.commitHomedir(m)
			fleetPage.dialogFieldActive = false
			fleetPage.syncEditFleetFocus()
			return cmd
		case "esc":
			// Discard the uncommitted edit; restore the persisted value.
			fleetPage.restoreHomedir(m)
			fleetPage.dialogFieldActive = false
			fleetPage.syncEditFleetFocus()
			return nil
		case "ctrl+c":
			fleetPage.closeEditFleet(m)
			return nil
		}
		if fleetPage.dialogRow == editFleetRowHomeDir {
			var cmd tea.Cmd
			fleetPage.homedirInput, cmd = fleetPage.homedirInput.Update(msg)
			return cmd
		}
		return nil
	}

	switch keyMsg.String() {
	case "up", "k":
		fleetPage.moveEditFleetRow(-1)
		return nil

	case "down", "j", "tab":
		fleetPage.moveEditFleetRow(1)
		return nil

	case "esc", "q", "Q", "ctrl+c":
		// An armed delete-cache confirm is cancelled first, not the dialog.
		if fleetPage.dialogDeleteCacheConfirm {
			fleetPage.dialogDeleteCacheConfirm = false
			return nil
		}
		fleetPage.closeEditFleet(m)
		return nil
	}

	// Row-specific actions.
	switch fleetPage.dialogRow {
	case editFleetRowClaude, editFleetRowCodex, editFleetRowGh, editFleetRowPreferFleetLaunch:
		// space/x and h/l/enter all toggle (instant-save), matching the
		// settings page.
		switch keyMsg.String() {
		case " ", "left", "right", "h", "l", "x", "enter":
			return fleetPage.toggleEditFleetRow(m)
		}
		return nil
	case editFleetRowCaching:
		switch keyMsg.String() {
		case " ", "enter":
			fleetPage.dialogCachingExpanded = !fleetPage.dialogCachingExpanded
		case "right", "l":
			fleetPage.dialogCachingExpanded = true
		case "left", "h":
			fleetPage.dialogCachingExpanded = false
		}
		return nil
	case editFleetRowBuildkit:
		return fleetPage.updateBuildkitRow(m, keyMsg)
	case editFleetRowHomeDir:
		switch keyMsg.String() {
		case "enter", " ":
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

// updateBuildkitRow handles the Buildkit server row inside the expanded Caching
// section: space/x toggles the setting (instant-save); when enabled, →/l focuses
// the [Delete cache] button and ←/h returns to the toggle; Enter on the button
// arms an inline confirm, and a second Enter performs the wipe asynchronously.
func (fleetPage *fleetPage) updateBuildkitRow(m *model, keyMsg tea.KeyMsg) tea.Cmd {
	// Ignore mutating keys while a wipe is in flight (navigation already ran).
	if fleetPage.dialogDeletingCache {
		return nil
	}
	switch keyMsg.String() {
	case " ", "x":
		// Toggling is a different action than confirming a delete, so always
		// disarm the confirm.
		fleetPage.dialogDeleteCacheConfirm = false
		cmd := fleetPage.toggleEditFleetRow(m)
		if !fleetPage.dialogBuildkitServer {
			// No server → no button; drop button focus too.
			fleetPage.dialogBuildkitButtonFocused = false
		}
		return cmd
	case "right", "l":
		if fleetPage.dialogBuildkitServer {
			fleetPage.dialogBuildkitButtonFocused = true
		}
		return nil
	case "left", "h":
		fleetPage.dialogBuildkitButtonFocused = false
		fleetPage.dialogDeleteCacheConfirm = false
		return nil
	case "enter":
		if fleetPage.dialogBuildkitButtonFocused && fleetPage.dialogBuildkitServer {
			if !fleetPage.dialogDeleteCacheConfirm {
				fleetPage.dialogDeleteCacheConfirm = true // first Enter: arm confirm
				return nil
			}
			fleetPage.dialogDeleteCacheConfirm = false // second Enter: do it
			fleetPage.dialogDeletingCache = true
			return deleteCacheCmd(fleetPage.dialogFleet)
		}
		// Toggle focused → toggle the setting.
		return fleetPage.toggleEditFleetRow(m)
	}
	return nil
}

// deleteCacheDoneMsg reports the outcome of a buildkit cache wipe.
type deleteCacheDoneMsg struct {
	fleet string
	err   error
}

// deleteCacheCmd runs the cache-wipe RPC off the UI loop and reports the result.
func deleteCacheCmd(fleetName string) tea.Cmd {
	return func() tea.Msg {
		return deleteCacheDoneMsg{fleet: fleetName, err: deleteBuildkitCacheRemote(fleetName)}
	}
}

// handleDeleteCacheDone clears the in-flight flag and surfaces the outcome.
func (fleetPage *fleetPage) handleDeleteCacheDone(m *model, msg deleteCacheDoneMsg) tea.Cmd {
	if fleetPage.dialogFleet == msg.fleet {
		fleetPage.dialogDeletingCache = false
	}
	if msg.err != nil {
		m.message = fmt.Sprintf("Delete cache failed: %v", msg.err)
	} else {
		m.message = "Build cache cleared"
	}
	return nil
}

// toggleEditFleetRow flips the boolean for the currently focused checkbox row
// and persists it immediately (reverting the flip if the save fails). When a
// home-dir mount is turned on it may also kick off auto-detection of the
// container's home directory.
func (fleetPage *fleetPage) toggleEditFleetRow(m *model) tea.Cmd {
	turnedOn := false
	var revert func()
	switch fleetPage.dialogRow {
	case editFleetRowClaude:
		fleetPage.dialogClaudeMount = !fleetPage.dialogClaudeMount
		turnedOn = fleetPage.dialogClaudeMount
		revert = func() { fleetPage.dialogClaudeMount = !fleetPage.dialogClaudeMount }
	case editFleetRowCodex:
		fleetPage.dialogCodexMount = !fleetPage.dialogCodexMount
		turnedOn = fleetPage.dialogCodexMount
		revert = func() { fleetPage.dialogCodexMount = !fleetPage.dialogCodexMount }
	case editFleetRowGh:
		fleetPage.dialogGhMount = !fleetPage.dialogGhMount
		turnedOn = fleetPage.dialogGhMount
		revert = func() { fleetPage.dialogGhMount = !fleetPage.dialogGhMount }
	case editFleetRowBuildkit:
		fleetPage.dialogBuildkitServer = !fleetPage.dialogBuildkitServer
		revert = func() { fleetPage.dialogBuildkitServer = !fleetPage.dialogBuildkitServer }
	case editFleetRowPreferFleetLaunch:
		prevSet := fleetPage.dialogPreferFleetLaunchSet
		fleetPage.dialogPreferFleetLaunch = !fleetPage.dialogPreferFleetLaunch
		// The user explicitly chose a value, so it must now persist.
		fleetPage.dialogPreferFleetLaunchSet = true
		// Revert BOTH the value and the set-flag on save failure — otherwise a
		// failed toggle would leave dialogPreferFleetLaunchSet=true and a later
		// unrelated save would collapse a "never asked" (nil) tri-state.
		revert = func() {
			fleetPage.dialogPreferFleetLaunch = !fleetPage.dialogPreferFleetLaunch
			fleetPage.dialogPreferFleetLaunchSet = prevSet
		}
	default:
		return nil
	}

	if err := fleetPage.persistFleetSettings(m); err != nil {
		if revert != nil {
			revert()
		}
		m.message = fmt.Sprintf("Failed to save: %v", err)
		return nil
	}

	// On enabling a home-dir mount with no home dir recorded yet, auto-detect it.
	if turnedOn {
		if f, ok := m.st.Fleets[fleetPage.dialogFleet]; ok && fleetPage.shouldKickHomedirDetect(f) {
			return fleetPage.startHomedirDetect(f)
		}
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

// handleHomedirDetected applies the result of an auto-detection and (instant
// save) persists the detected value. The guard checks ensure stale results —
// from a fleet the user has since closed, or arriving after the user has
// already typed a value — never overwrite live state.
func (fleetPage *fleetPage) handleHomedirDetected(m *model, msg homedirDetectedMsg) tea.Cmd {
	// Always clear the in-flight flag for *this* fleet so the spinner
	// stops, even when the result is not applied.
	if fleetPage.dialogFleet == msg.fleetName {
		fleetPage.dialogDetecting = false
	}
	if msg.err != nil || msg.homeDir == "" {
		return nil
	}
	if fleetPage.mode != viewEditFleet || fleetPage.dialogFleet != msg.fleetName {
		return nil
	}
	if strings.TrimSpace(fleetPage.homedirInput.Value()) != "" {
		return nil
	}
	fleetPage.homedirInput.SetValue(msg.homeDir)
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.restoreHomedir(m)
		m.message = fmt.Sprintf("Failed to save: %v", err)
	}
	return nil
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

// persistFleetSettings writes the dialog's current state to the fleet record
// and saves it through the server (instant-save). On RPC failure it reverts the
// fleet record to its prior settings and returns the error so the caller can
// undo its optimistic change too. Existing instances are not retroactively
// re-mounted; settings apply to the next instance provisioned on a supporting
// backend.
//
// PreferFleetLaunch is only written when the fleet already had a value or the
// user toggled it this session (dialogPreferFleetLaunchSet) — so editing an
// unrelated setting never collapses a "never asked" (nil) into explicit false.
func (fleetPage *fleetPage) persistFleetSettings(m *model) error {
	f, ok := m.st.Fleets[fleetPage.dialogFleet]
	if !ok {
		return fmt.Errorf("fleet %s not found", fleetPage.dialogFleet)
	}
	prev := f.Settings
	f.Settings.ClaudeCodeMount = fleetPage.dialogClaudeMount
	f.Settings.CodexMount = fleetPage.dialogCodexMount
	f.Settings.GhMount = fleetPage.dialogGhMount
	f.Settings.BuildkitServer = fleetPage.dialogBuildkitServer
	if fleetPage.dialogPreferFleetLaunchSet {
		preferFleetLaunch := fleetPage.dialogPreferFleetLaunch
		f.Settings.PreferFleetLaunch = &preferFleetLaunch
	}
	f.Settings.HomeDir = strings.TrimSpace(fleetPage.homedirInput.Value())

	if err := setFleetSettingsRemote(fleetPage.dialogFleet, f.Settings); err != nil {
		f.Settings = prev
		return err
	}
	return nil
}

// commitHomedir persists the current home-dir input value (instant-save),
// restoring the input to the saved value if the save fails.
func (fleetPage *fleetPage) commitHomedir(m *model) tea.Cmd {
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.restoreHomedir(m)
		m.message = fmt.Sprintf("Failed to save: %v", err)
	}
	return nil
}

// restoreHomedir resets the home-dir input to the fleet's persisted value (used
// to discard an uncommitted edit or undo a failed save).
func (fleetPage *fleetPage) restoreHomedir(m *model) {
	if f, ok := m.st.Fleets[fleetPage.dialogFleet]; ok {
		fleetPage.homedirInput.SetValue(f.Settings.HomeDir)
	}
}

// closeEditFleet closes the dialog. Instant-save means there is nothing to
// commit on close — every change was persisted as it was made.
func (fleetPage *fleetPage) closeEditFleet(_ *model) {
	fleetPage.mode = viewNormal
	fleetPage.dialogDeleteCacheConfirm = false
	fleetPage.dialogBuildkitButtonFocused = false
	// Clear the in-flight flag too: a wipe RPC may still be running, but its
	// deleteCacheDoneMsg is matched on fleet name and only updates the message,
	// so the dialog must not reopen showing a stale "Clearing…" spinner.
	fleetPage.dialogDeletingCache = false
	fleetPage.blurDialogFields()
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

	// The server owns backend access: it returns the forward command argv and
	// the client runs it. A nil ResolveFunc skips the in-process direct-host
	// fast path (the server resolves hostnames), matching the CLI's behaviour.
	argv, err := portForwardArgvTUI(fleetPage.dialogFleet, fleetPage.dialogInst, local, remote)
	if err != nil {
		m.message = err.Error()
		return nil
	}
	if len(argv) == 0 {
		m.message = "server returned no port-forward command"
		return nil
	}
	cmdFn := func(_ string, _, _ int) *exec.Cmd { return exec.Command(argv[0], argv[1:]...) }
	if err := m.portForwards.Add(key, local, remote, cmdFn, fleetPage.pfContainerID, nil); err != nil {
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
			return execProcess(
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
	return createSessionCmd(ref, fullName)
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

	if isGrouped {
		return renameGroupCmd(ref, sanitized, oldGID, newName)
	}
	return renameSessionCmd(ref, oldName, newName)
}
