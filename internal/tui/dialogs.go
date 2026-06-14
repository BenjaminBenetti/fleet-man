package tui

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/devcontainersetup"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
	devcontainercheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/devcontainer"
	homedircheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/homedir"
	tea "github.com/charmbracelet/bubbletea"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
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

// updateConfirmRebuild handles the instance rebuild confirmation dialog.
// Rebuild recreates the container in place (preserving the workspace), so it is
// a single-step confirm — no double warning like fleet delete.
func (fleetPage *fleetPage) updateConfirmRebuild(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y", "enter":
			// Rebuild runs as a server job. Flip an optimistic in-memory
			// Rebuilding status for the spinner (NOT persisted — the server owns
			// the reprovision and the persisted status); operationDoneMsg reload()s
			// the authoritative result.
			f, ok := m.st.Fleets[fleetPage.dialogFleet]
			if ok {
				instance, err := f.GetInstance(fleetPage.dialogInst)
				if err == nil {
					instance.Status = fleet.StatusRebuilding
					fleetPage.buildRows(m)
					fleetPage.mode = viewNormal
					return rebuildInstanceCmd(fleetPage.dialogFleet, fleetPage.dialogInst)
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
			return switchBrowserCmd(m.portForwards, instanceKey, dataDir, f.Settings.PreferFleetLaunchEnabled(), "")
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

// updateArmadaSelect handles the Armada dropdown (opened from the selector on
// the list box's top border): j/k move, enter switches the TUI's active fleetd
// connection to the chosen entry, esc cancels. Selecting the current entry is
// a no-op — registration and selection are deliberately separate.
func (fleetPage *fleetPage) updateArmadaSelect(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	entries := m.armadaEntries()
	n := len(entries)
	switch keyMsg.String() {
	case "esc", "q", "ctrl+c":
		fleetPage.mode = viewNormal
		return nil
	case "up", "k":
		fleetPage.armadaDialogRow = (fleetPage.armadaDialogRow - 1 + n) % n
		return nil
	case "down", "j", "tab":
		fleetPage.armadaDialogRow = (fleetPage.armadaDialogRow + 1) % n
		return nil
	case "enter", " ":
		fleetPage.mode = viewNormal
		entry := entries[min(fleetPage.armadaDialogRow, n-1)]
		if entry.current {
			m.message = "Already connected to " + entry.displayName
			return nil
		}
		return m.switchArmada(entry)
	}
	return nil
}

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
	fleetPage.dialogAddingMount = false
	fleetPage.textInput.Blur()
	fleetPage.branchInput.Blur()
	fleetPage.homedirInput.Blur()
	fleetPage.customMountInput.Blur()
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
	// Keep the dialog open so the user can correct the name in place.
	if err := fleet.ValidateInstanceName(name); err != nil {
		m.message = err.Error()
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
	if err := fleet.ValidateInstanceName(displayName); err != nil {
		m.message = err.Error()
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
	// The tag renders as its own row under an expanded instance, so the
	// row list must be rebuilt for the change to show immediately.
	fleetPage.buildRows(m)
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

// inspectDevcontainerCmd asks the SERVER to clone the repo and check for a
// devcontainer config, in a background goroutine. Inspection runs on the
// daemon's host — the machine that will actually clone at provision time — so
// a remote TUI gets the verdict of the daemon's git credentials, not its own
// (issue #141 note 5).
//
// A clone failure surfaces with err set so the caller can report it
// rather than blindly assuming the repo lacks a devcontainer — an
// unreachable URL is a different problem than a configured-but-missing
// devcontainer, and the user almost certainly wants to fix the URL
// before being offered a setup workflow.
func inspectDevcontainerCmd(fleetName, remoteURL string) tea.Cmd {
	return func() tea.Msg {
		reply, err := inspectRepoRemote(remoteURL, "", false)
		if grpcstatus.Code(err) == grpccodes.Unimplemented {
			// Compatibility fallback for daemons that predate InspectRepo:
			// clone + check locally like the TUI always used to.
			return inspectDevcontainerLocal(fleetName, remoteURL)
		}
		if err != nil {
			// Unwrap the status so the user sees the clone error itself, not
			// the "rpc error: code = ..." framing around it.
			return devcontainerInspectedMsg{fleetName: fleetName, err: errors.New(grpcstatus.Convert(err).Message())}
		}
		return devcontainerInspectedMsg{
			fleetName:       fleetName,
			hasDevcontainer: reply.GetHasDevcontainer(),
		}
	}
}

// inspectDevcontainerLocal is the pre-InspectRepo behavior — a shallow clone
// with THIS process's credentials — kept only as the compatibility fallback
// above. The Repo handle is closed before the message is returned so the temp
// clone never outlives the command.
func inspectDevcontainerLocal(fleetName, remoteURL string) tea.Msg {
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
	editFleetRowAuggie
	editFleetRowHomeDir
	editFleetRowPreferFleetLaunch
	editFleetRowLayouts      // collapsible section header (issue #150)
	editFleetRowCustomMounts // collapsible section header
	editFleetRowCaching      // collapsible section header
	editFleetRowBuildkit     // child of Caching; only navigable when expanded
	editFleetRowDebCache     // child of Caching; only navigable when expanded
	editFleetRowImageCache   // child of Caching; only navigable when expanded
	editFleetRowCount
)

// editFleetRowCustomMountBase is the start of the dynamic custom-mount child
// rows, placed well above the fixed row constants so the two never collide.
// Row editFleetRowCustomMountBase+i is the i-th existing custom mount; the row
// at base+len(customMounts) is the "+ Add mount" affordance.
const editFleetRowCustomMountBase = 1000

// editFleetRowLayoutPresetBase is the start of the dynamic layout-preset child
// rows, a second dynamic band above the custom-mount one. Row base+i is the
// i-th existing preset; the row at base+len(presets) is "+ Layout Preset".
const editFleetRowLayoutPresetBase = 2000

// isCustomMountChildRow reports whether row is one of the dynamic custom-mount
// child rows (an existing mount or the "+ Add mount" row).
func isCustomMountChildRow(row int) bool {
	return row >= editFleetRowCustomMountBase && row < editFleetRowLayoutPresetBase
}

// isLayoutPresetChildRow reports whether row is one of the dynamic
// layout-preset child rows (an existing preset or the "+ Layout Preset" row).
func isLayoutPresetChildRow(row int) bool { return row >= editFleetRowLayoutPresetBase }

// customMountAddRow returns the row id of the "+ Add mount" affordance, which
// always sits just past the last existing custom mount.
func (fleetPage *fleetPage) customMountAddRow() int {
	return editFleetRowCustomMountBase + len(fleetPage.dialogCustomMounts)
}

// layoutPresetAddRow returns the row id of the "+ Layout Preset" affordance,
// which always sits just past the last existing preset.
func (fleetPage *fleetPage) layoutPresetAddRow() int {
	return editFleetRowLayoutPresetBase + len(fleetPage.dialogLayoutPresets)
}

// cacheKind identifies one of the three caches that share the Caching section's
// toggle + [Delete cache] interaction model.
type cacheKind int

const (
	cacheBuildkit cacheKind = iota
	cacheDeb
	cacheImage
)

// cacheKindForRow maps a dialog row to its cache kind, reporting false for rows
// that are not cache rows.
func cacheKindForRow(row int) (cacheKind, bool) {
	switch row {
	case editFleetRowBuildkit:
		return cacheBuildkit, true
	case editFleetRowDebCache:
		return cacheDeb, true
	case editFleetRowImageCache:
		return cacheImage, true
	}
	return 0, false
}

// cacheEnabled reports the in-dialog toggle state for a cache kind.
func (fleetPage *fleetPage) cacheEnabled(k cacheKind) bool {
	switch k {
	case cacheBuildkit:
		return fleetPage.dialogBuildkitServer
	case cacheDeb:
		return fleetPage.dialogDebCache
	case cacheImage:
		return fleetPage.dialogImageCache
	}
	return false
}

// enabledCacheCount returns how many of the three caches (buildkit / deb / image)
// are currently toggled on, for the count shown in the Caching section header.
func (fleetPage *fleetPage) enabledCacheCount() int {
	n := 0
	for _, k := range []cacheKind{cacheBuildkit, cacheDeb, cacheImage} {
		if fleetPage.cacheEnabled(k) {
			n++
		}
	}
	return n
}

// cacheRowFocused reports whether the dialog cursor is currently on the row for
// cache kind k (used so the [Delete cache] button only highlights its own row).
func (fleetPage *fleetPage) cacheRowFocused(k cacheKind) bool {
	rk, ok := cacheKindForRow(fleetPage.dialogRow)
	return ok && rk == k
}

// visibleEditFleetRows returns the edit-fleet dialog's navigable rows in display
// order. The custom-mount child rows appear only while that section is expanded
// (one per mount, then the add row); the Buildkit row only appears while the
// Caching section is expanded.
func (fleetPage *fleetPage) visibleEditFleetRows() []int {
	rows := []int{
		editFleetRowClaude,
		editFleetRowCodex,
		editFleetRowGh,
		editFleetRowAuggie,
		editFleetRowHomeDir,
		editFleetRowPreferFleetLaunch,
		editFleetRowLayouts,
	}
	if fleetPage.dialogLayoutsExpanded {
		for i := range fleetPage.dialogLayoutPresets {
			rows = append(rows, editFleetRowLayoutPresetBase+i)
		}
		rows = append(rows, fleetPage.layoutPresetAddRow())
	}
	rows = append(rows, editFleetRowCustomMounts)
	if fleetPage.dialogCustomMountsExpanded {
		for i := range fleetPage.dialogCustomMounts {
			rows = append(rows, editFleetRowCustomMountBase+i)
		}
		rows = append(rows, fleetPage.customMountAddRow())
	}
	rows = append(rows, editFleetRowCaching)
	if fleetPage.dialogCachingExpanded {
		rows = append(rows, editFleetRowBuildkit, editFleetRowDebCache, editFleetRowImageCache)
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
	fleetPage.dialogCacheButtonFocused = false
	fleetPage.dialogDeleteCacheConfirm = false
	fleetPage.dialogMountRemoveConfirm = false
	fleetPage.dialogPresetRemoveFocused = false
	fleetPage.dialogPresetRemoveConfirm = false
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

// detectHomedirCmd asks the SERVER to clone the fleet's remote and run the
// home-dir check, in a background goroutine. Detection runs on the daemon's
// host (issue #141 note 5) — deliberately so: the check may docker-pull the
// devcontainer image, and the daemon's docker is the one provisioning uses.
//
// Errors are surfaced as part of homedirDetectedMsg; the caller
// treats them the same as a successful empty result (spinner stops,
// nothing populated) because failure is expected (no devcontainer.json,
// missing docker, network blocked, …) and the user can always type a
// path manually. A reply with an empty homeDir is the server's "no hint"
// answer and maps to the same outcome — handleHomedirDetected ignores it.
func detectHomedirCmd(fleetName, remoteURL, branch string) tea.Cmd {
	return func() tea.Msg {
		reply, err := inspectRepoRemote(remoteURL, branch, true)
		if grpcstatus.Code(err) == grpccodes.Unimplemented {
			// Compatibility fallback for daemons that predate InspectRepo:
			// clone + detect locally like the TUI always used to.
			return detectHomedirLocal(fleetName, remoteURL, branch)
		}
		if err != nil {
			return homedirDetectedMsg{fleetName: fleetName, err: err}
		}
		return homedirDetectedMsg{fleetName: fleetName, homeDir: reply.GetHomeDir()}
	}
}

// detectHomedirLocal is the pre-InspectRepo behavior — a local clone + check
// with THIS process's credentials/docker — kept only as the compatibility
// fallback above. The handle is closed before the message is returned so the
// temp clone never outlives the command.
func detectHomedirLocal(fleetName, remoteURL, branch string) tea.Msg {
	repo, err := inspector.Open(remoteURL, branch)
	if err != nil {
		return homedirDetectedMsg{fleetName: fleetName, err: err}
	}
	defer repo.Close()
	homeDir, err := homedircheck.Detect(repo)
	return homedirDetectedMsg{fleetName: fleetName, homeDir: homeDir, err: err}
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
	fleetPage.dialogAuggieMount = f.Settings.AuggieMount
	fleetPage.dialogBuildkitServer = f.Settings.BuildkitServer
	fleetPage.dialogDebCache = f.Settings.DebCacheServer
	fleetPage.dialogImageCache = f.Settings.ImageCacheServer
	fleetPage.dialogPreferFleetLaunch = f.Settings.PreferFleetLaunchEnabled()
	fleetPage.dialogPreferFleetLaunchSet = f.Settings.PreferFleetLaunchSet()
	fleetPage.dialogRow = editFleetRowClaude
	fleetPage.dialogDetecting = false
	fleetPage.dialogFieldActive = false
	fleetPage.dialogCachingExpanded = false
	fleetPage.dialogCacheButtonFocused = false
	fleetPage.dialogDeleteCacheConfirm = false
	fleetPage.dialogDeleting = false
	fleetPage.dialogCustomMountsExpanded = false
	fleetPage.dialogCustomMounts = slices.Clone(f.Settings.CustomMounts)
	fleetPage.dialogAddingMount = false
	fleetPage.dialogCustomMountErr = ""
	fleetPage.dialogMountRemoveConfirm = false
	fleetPage.customMountInput.SetValue("")
	fleetPage.customMountInput.Blur()
	fleetPage.dialogLayoutsExpanded = false
	fleetPage.dialogLayoutPresets = slices.Clone(f.Settings.LayoutPresets)
	fleetPage.dialogPresetRemoveFocused = false
	fleetPage.dialogPresetRemoveConfirm = false
	fleetPage.lpFlow = nil

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

	// Add-custom-mount text-editing sub-mode.
	if fleetPage.dialogAddingMount {
		switch keyMsg.String() {
		case "enter":
			return fleetPage.commitNewMount(m)
		case "esc":
			fleetPage.cancelAddMount()
			return nil
		case "ctrl+c":
			fleetPage.closeEditFleet(m)
			return nil
		}
		// Any other key edits the field; clear a stale validation error as the
		// user types so the inline message tracks the current input.
		fleetPage.dialogCustomMountErr = ""
		var cmd tea.Cmd
		fleetPage.customMountInput, cmd = fleetPage.customMountInput.Update(msg)
		return cmd
	}

	switch keyMsg.String() {
	case "up", "k":
		fleetPage.moveEditFleetRow(-1)
		return nil

	case "down", "j", "tab":
		fleetPage.moveEditFleetRow(1)
		return nil

	case "esc", "q", "Q", "ctrl+c":
		// An armed confirm (delete-cache or remove-mount) is cancelled first,
		// not the dialog.
		if fleetPage.dialogDeleteCacheConfirm {
			fleetPage.dialogDeleteCacheConfirm = false
			return nil
		}
		if fleetPage.dialogMountRemoveConfirm {
			fleetPage.dialogMountRemoveConfirm = false
			return nil
		}
		if fleetPage.dialogPresetRemoveConfirm {
			fleetPage.dialogPresetRemoveConfirm = false
			return nil
		}
		fleetPage.closeEditFleet(m)
		return nil
	}

	// Dynamic custom-mount child rows (existing mounts + the add row) are
	// handled separately since their row ids are not compile-time constants.
	if isCustomMountChildRow(fleetPage.dialogRow) {
		return fleetPage.updateCustomMountRow(m, keyMsg)
	}
	// Likewise the dynamic layout-preset child rows (existing presets + the
	// "+ Layout Preset" row).
	if isLayoutPresetChildRow(fleetPage.dialogRow) {
		return fleetPage.updateLayoutPresetRow(m, keyMsg)
	}

	// Row-specific actions.
	switch fleetPage.dialogRow {
	case editFleetRowClaude, editFleetRowCodex, editFleetRowGh, editFleetRowAuggie, editFleetRowPreferFleetLaunch:
		// space/x and h/l/enter all toggle (instant-save), matching the
		// settings page.
		switch keyMsg.String() {
		case " ", "left", "right", "h", "l", "x", "enter":
			return fleetPage.toggleEditFleetRow(m)
		}
		return nil
	case editFleetRowCustomMounts:
		switch keyMsg.String() {
		case " ", "enter":
			fleetPage.dialogCustomMountsExpanded = !fleetPage.dialogCustomMountsExpanded
		case "right", "l":
			fleetPage.dialogCustomMountsExpanded = true
		case "left", "h":
			fleetPage.dialogCustomMountsExpanded = false
		}
		return nil
	case editFleetRowLayouts:
		switch keyMsg.String() {
		case " ", "enter":
			fleetPage.dialogLayoutsExpanded = !fleetPage.dialogLayoutsExpanded
		case "right", "l":
			fleetPage.dialogLayoutsExpanded = true
		case "left", "h":
			fleetPage.dialogLayoutsExpanded = false
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
		return fleetPage.updateCacheRow(m, keyMsg, cacheBuildkit)
	case editFleetRowDebCache:
		return fleetPage.updateCacheRow(m, keyMsg, cacheDeb)
	case editFleetRowImageCache:
		return fleetPage.updateCacheRow(m, keyMsg, cacheImage)
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

// updateCacheRow handles a cache row (buildkit / deb / image) inside the
// expanded Caching section: space/x toggles the setting (instant-save); when
// enabled, →/l focuses the [Delete cache] button and ←/h returns to the toggle;
// Enter on the button arms an inline confirm, and a second Enter performs the
// wipe asynchronously. The shared per-row sub-state (button focus / confirm)
// applies to whichever cache row currently has the cursor.
func (fleetPage *fleetPage) updateCacheRow(m *model, keyMsg tea.KeyMsg, k cacheKind) tea.Cmd {
	// Ignore mutating keys while ANY wipe is in flight (navigation already ran),
	// so a second wipe can't be started before the first reports back.
	if fleetPage.dialogDeleting {
		return nil
	}
	switch keyMsg.String() {
	case " ", "x":
		// Toggling is a different action than confirming a delete, so always
		// disarm the confirm.
		fleetPage.dialogDeleteCacheConfirm = false
		cmd := fleetPage.toggleEditFleetRow(m)
		if !fleetPage.cacheEnabled(k) {
			// No server → no button; drop button focus too.
			fleetPage.dialogCacheButtonFocused = false
		}
		return cmd
	case "right", "l":
		if fleetPage.cacheEnabled(k) {
			fleetPage.dialogCacheButtonFocused = true
		}
		return nil
	case "left", "h":
		fleetPage.dialogCacheButtonFocused = false
		fleetPage.dialogDeleteCacheConfirm = false
		return nil
	case "enter":
		if fleetPage.dialogCacheButtonFocused && fleetPage.cacheEnabled(k) {
			if !fleetPage.dialogDeleteCacheConfirm {
				fleetPage.dialogDeleteCacheConfirm = true // first Enter: arm confirm
				return nil
			}
			fleetPage.dialogDeleteCacheConfirm = false // second Enter: do it
			fleetPage.dialogDeleting = true
			fleetPage.dialogDeletingKind = k
			return deleteCacheCmd(k, fleetPage.dialogFleet)
		}
		// Toggle focused → toggle the setting.
		return fleetPage.toggleEditFleetRow(m)
	}
	return nil
}

// deleteCacheDoneMsg reports the outcome of a cache wipe (which cache via kind).
type deleteCacheDoneMsg struct {
	fleet string
	kind  cacheKind
	err   error
}

// deleteCacheCmd runs the cache-wipe RPC for kind k off the UI loop and reports
// the result, dispatching to the matching server RPC.
func deleteCacheCmd(k cacheKind, fleetName string) tea.Cmd {
	return func() tea.Msg {
		var err error
		switch k {
		case cacheBuildkit:
			err = deleteBuildkitCacheRemote(fleetName)
		case cacheDeb:
			err = deleteDebCacheRemote(fleetName)
		case cacheImage:
			err = deleteImageCacheRemote(fleetName)
		}
		return deleteCacheDoneMsg{fleet: fleetName, kind: k, err: err}
	}
}

// handleDeleteCacheDone clears the in-flight flag and surfaces the outcome.
func (fleetPage *fleetPage) handleDeleteCacheDone(m *model, msg deleteCacheDoneMsg) tea.Cmd {
	if fleetPage.dialogFleet == msg.fleet && fleetPage.dialogDeletingKind == msg.kind {
		fleetPage.dialogDeleting = false
	}
	if msg.err != nil {
		m.message = fmt.Sprintf("Delete cache failed: %v", msg.err)
	} else {
		m.message = cacheClearedMessage(msg.kind)
	}
	return nil
}

// cacheClearedMessage is the success banner for a cache wipe.
func cacheClearedMessage(k cacheKind) string {
	switch k {
	case cacheDeb:
		return "Deb cache cleared"
	case cacheImage:
		return "Image cache cleared"
	default:
		return "Build cache cleared"
	}
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
	case editFleetRowAuggie:
		fleetPage.dialogAuggieMount = !fleetPage.dialogAuggieMount
		turnedOn = fleetPage.dialogAuggieMount
		revert = func() { fleetPage.dialogAuggieMount = !fleetPage.dialogAuggieMount }
	case editFleetRowBuildkit:
		fleetPage.dialogBuildkitServer = !fleetPage.dialogBuildkitServer
		revert = func() { fleetPage.dialogBuildkitServer = !fleetPage.dialogBuildkitServer }
	case editFleetRowDebCache:
		fleetPage.dialogDebCache = !fleetPage.dialogDebCache
		revert = func() { fleetPage.dialogDebCache = !fleetPage.dialogDebCache }
	case editFleetRowImageCache:
		fleetPage.dialogImageCache = !fleetPage.dialogImageCache
		revert = func() { fleetPage.dialogImageCache = !fleetPage.dialogImageCache }
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

// updateCustomMountRow handles a key press while the cursor is on one of the
// dynamic custom-mount child rows: an existing mount (enter/x/d removes it,
// instant-save) or the "+ Add mount" row (enter or the first typed character
// opens the inline text input).
func (fleetPage *fleetPage) updateCustomMountRow(m *model, keyMsg tea.KeyMsg) tea.Cmd {
	idx := fleetPage.dialogRow - editFleetRowCustomMountBase
	if idx == len(fleetPage.dialogCustomMounts) {
		// The "+ Add mount" row.
		switch keyMsg.String() {
		case "enter", " ":
			fleetPage.beginAddMount()
			return fleetPage.customMountInput.Cursor.BlinkCmd()
		}
		// Start typing immediately, like the home-dir row does.
		if isDialogTextKey(keyMsg) {
			fleetPage.beginAddMount()
			blinkCmd := fleetPage.customMountInput.Cursor.BlinkCmd()
			var inputCmd tea.Cmd
			fleetPage.customMountInput, inputCmd = fleetPage.customMountInput.Update(keyMsg)
			return tea.Batch(blinkCmd, inputCmd)
		}
		return nil
	}
	// An existing mount row: removal is a two-step confirm (mirroring the
	// Caching section's [Delete cache] button) so a stray Enter can't silently
	// drop a mount. The first enter/x/d arms the inline "[remove?]" confirm; the
	// second enter/x/d performs the removal (instant-save). Esc disarms it via
	// the dialog's top-level esc handler, and any row move clears it.
	switch keyMsg.String() {
	case "enter", "x", "d":
		if !fleetPage.dialogMountRemoveConfirm {
			fleetPage.dialogMountRemoveConfirm = true // first press: arm confirm
			return nil
		}
		fleetPage.dialogMountRemoveConfirm = false // second press: do it
		return fleetPage.removeCustomMount(m, idx)
	}
	return nil
}

// updateLayoutPresetRow handles a key press while the cursor is on one of the
// dynamic layout-preset child rows. An existing preset row works exactly like a
// Caching cache row: the row's primary action (Enter) opens the editor, and the
// [remove] button is a horizontal sub-cursor reached with →/l — Enter there
// arms a "[remove?]" confirm, a second Enter performs the removal, and ←/h
// returns to the row. The "+ Layout Preset" row just starts the capture flow.
func (fleetPage *fleetPage) updateLayoutPresetRow(m *model, keyMsg tea.KeyMsg) tea.Cmd {
	idx := fleetPage.dialogRow - editFleetRowLayoutPresetBase
	if idx == len(fleetPage.dialogLayoutPresets) {
		// The "+ Layout Preset" row.
		switch keyMsg.String() {
		case "enter", " ":
			fleetPage.openLayoutPresetCreate(m)
		}
		return nil
	}
	switch keyMsg.String() {
	case "right", "l":
		fleetPage.dialogPresetRemoveFocused = true
		return nil
	case "left", "h":
		fleetPage.dialogPresetRemoveFocused = false
		fleetPage.dialogPresetRemoveConfirm = false
		return nil
	case "enter", " ":
		if fleetPage.dialogPresetRemoveFocused {
			if !fleetPage.dialogPresetRemoveConfirm {
				fleetPage.dialogPresetRemoveConfirm = true // first Enter: arm confirm
				return nil
			}
			fleetPage.dialogPresetRemoveConfirm = false // second Enter: remove
			return fleetPage.removeLayoutPreset(m, idx)
		}
		// Row focused (not the remove button) → open the editor.
		fleetPage.openLayoutPresetEdit(idx)
		return nil
	}
	return nil
}

// removeLayoutPreset drops the idx-th preset and persists (instant-save),
// reverting on RPC failure and keeping the cursor in range afterward.
func (fleetPage *fleetPage) removeLayoutPreset(m *model, idx int) tea.Cmd {
	if idx < 0 || idx >= len(fleetPage.dialogLayoutPresets) {
		return nil
	}
	prev := fleetPage.dialogLayoutPresets
	fleetPage.dialogLayoutPresets = slices.Delete(slices.Clone(prev), idx, idx+1)
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.dialogLayoutPresets = prev
		m.message = fmt.Sprintf("Failed to save: %v", err)
		return nil
	}
	if fleetPage.dialogRow > fleetPage.layoutPresetAddRow() {
		fleetPage.dialogRow = fleetPage.layoutPresetAddRow()
	}
	return nil
}

// beginAddMount enters the add-custom-mount text sub-mode with a blank input.
func (fleetPage *fleetPage) beginAddMount() {
	fleetPage.dialogAddingMount = true
	fleetPage.dialogCustomMountErr = ""
	fleetPage.customMountInput.SetValue("")
	fleetPage.customMountInput.Focus()
}

// cancelAddMount leaves the add-custom-mount sub-mode, discarding the input.
func (fleetPage *fleetPage) cancelAddMount() {
	fleetPage.dialogAddingMount = false
	fleetPage.dialogCustomMountErr = ""
	fleetPage.customMountInput.SetValue("")
	fleetPage.customMountInput.Blur()
}

// commitNewMount validates the typed path, appends it to the working list and
// persists (instant-save). On a validation failure the inline error is set and
// the sub-mode stays open so the user can fix the value; on an RPC failure the
// optimistic append is reverted.
func (fleetPage *fleetPage) commitNewMount(m *model) tea.Cmd {
	norm, err := fleet.NormalizeCustomMount(fleetPage.customMountInput.Value())
	if err != nil {
		fleetPage.dialogCustomMountErr = err.Error()
		return nil
	}
	// Last-wins collisions with managed mounts are allowed, but an exact repeat
	// of an existing custom mount is a no-op — reject it with a clear message
	// rather than silently dropping it.
	if slices.Contains(fleetPage.dialogCustomMounts, norm) {
		fleetPage.dialogCustomMountErr = fmt.Sprintf("%s is already mounted", norm)
		return nil
	}

	prev := fleetPage.dialogCustomMounts
	fleetPage.dialogCustomMounts = append(slices.Clone(prev), norm)
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.dialogCustomMounts = prev
		m.message = fmt.Sprintf("Failed to save: %v", err)
		return nil
	}
	// Leave the cursor on the (now shifted-down) add row so the user can keep
	// adding mounts in a row.
	fleetPage.dialogRow = fleetPage.customMountAddRow()
	fleetPage.cancelAddMount()
	return nil
}

// removeCustomMount drops the idx-th custom mount and persists (instant-save),
// reverting on RPC failure and keeping the cursor in range afterward.
func (fleetPage *fleetPage) removeCustomMount(m *model, idx int) tea.Cmd {
	if idx < 0 || idx >= len(fleetPage.dialogCustomMounts) {
		return nil
	}
	prev := fleetPage.dialogCustomMounts
	fleetPage.dialogCustomMounts = slices.Delete(slices.Clone(prev), idx, idx+1)
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.dialogCustomMounts = prev
		m.message = fmt.Sprintf("Failed to save: %v", err)
		return nil
	}
	// Removing the last mount shrinks the visible row range; keep the cursor on
	// a row that still exists (at most the add row).
	if fleetPage.dialogRow > fleetPage.customMountAddRow() {
		fleetPage.dialogRow = fleetPage.customMountAddRow()
	}
	return nil
}

// customMountHostPreview renders the host path a custom mount resolves to, for
// display under the dialog. Mirrors the resolver's derivation
// (~/.fleet/workspaces/<fleet>/.mnt/<path>) using the original container path.
func customMountHostPreview(fleetName, containerPath string) string {
	sub := strings.TrimPrefix(filepath.Clean(strings.TrimSpace(containerPath)), "/")
	return filepath.Join("~/.fleet/workspaces", fleetName, ".mnt", sub)
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
	if !fleetPage.dialogClaudeMount && !fleetPage.dialogCodexMount && !fleetPage.dialogGhMount && !fleetPage.dialogAuggieMount {
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
	f.Settings.AuggieMount = fleetPage.dialogAuggieMount
	f.Settings.BuildkitServer = fleetPage.dialogBuildkitServer
	f.Settings.CustomMounts = fleetPage.dialogCustomMounts
	f.Settings.LayoutPresets = fleetPage.dialogLayoutPresets
	f.Settings.DebCacheServer = fleetPage.dialogDebCache
	f.Settings.ImageCacheServer = fleetPage.dialogImageCache
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
	fleetPage.dialogCacheButtonFocused = false
	// Clear the in-flight flag too: a wipe RPC may still be running, but its
	// deleteCacheDoneMsg is matched on fleet name and only updates the message,
	// so the dialog must not reopen showing a stale "Clearing…" spinner.
	fleetPage.dialogDeleting = false
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

	// The server owns backend access: each accepted connection is tunnelled
	// to the instance over the server's Forward stream (the data plane), so
	// the forward works even against a remote server.
	dial, err := forwardDialer(fleetPage.dialogFleet, fleetPage.dialogInst, remote)
	if err != nil {
		m.message = err.Error()
		return nil
	}
	if err := m.portForwards.Add(key, local, remote, dial); err != nil {
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
// openCreateSessionDialog opens the new-session dialog for an instance,
// snapshotting the fleet's layout presets for Tab cycling (none selected by
// default — a plain single session).
func (fleetPage *fleetPage) openCreateSessionDialog(m *model, fleetName string, instance *fleet.Instance) tea.Cmd {
	fleetPage.mode = viewCreateSession
	fleetPage.dialogFleet = fleetName
	fleetPage.dialogInst = instance.Name
	fleetPage.dialogPresets = nil
	if f, ok := m.st.Fleets[fleetName]; ok {
		fleetPage.dialogPresets = f.Settings.LayoutPresets
	}
	fleetPage.dialogPresetIdx = -1
	fleetPage.textInput.SetValue("")
	fleetPage.textInput.Placeholder = "session-name (or empty for auto)"
	fleetPage.textInput.CharLimit = 64
	return fleetPage.activateTextInput()
}

// cyclePresetTemplate steps the new-session dialog's template selection:
// -1 (none) → preset 0 → … → preset N-1 → back to none, mirroring how the
// add-instance dialog Tab-cycles backends.
func (fleetPage *fleetPage) cyclePresetTemplate(direction int) {
	n := len(fleetPage.dialogPresets)
	if n == 0 {
		return
	}
	// Shift the -1..n-1 range to 0..n and cycle modulo n+1.
	fleetPage.dialogPresetIdx = (fleetPage.dialogPresetIdx+1+direction+n+1)%(n+1) - 1
}

func (fleetPage *fleetPage) updateCreateSession(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Tab cycles the layout-preset template in both navigation and
		// text-entry states (the dialog's only text field never uses Tab).
		switch msg.String() {
		case "tab":
			fleetPage.cyclePresetTemplate(1)
			return nil
		case "shift+tab":
			fleetPage.cyclePresetTemplate(-1)
			return nil
		}

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

	// Resolve the Tab-selected template (if any) up front so an empty name can
	// default to the template's name rather than the generic "session-N".
	var preset *fleet.LayoutPreset
	if fleetPage.dialogPresetIdx >= 0 && fleetPage.dialogPresetIdx < len(fleetPage.dialogPresets) {
		preset = &fleetPage.dialogPresets[fleetPage.dialogPresetIdx]
	}

	if name == "" {
		if preset != nil {
			name = preset.Name // default to the template's name
		} else {
			name = nextSessionName(m.sessionStore.Sessions(ref))
		}
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
	fullName := GroupSessionName(sanitized, name)

	// A template was Tab-selected: mint the whole group — one session per
	// pane, the resolved name as the group ID — and run each pane's command.
	if preset != nil {
		sessions := make([]string, 0, preset.PaneCount())
		sessions = append(sessions, fullName)
		for i := 1; i < preset.PaneCount(); i++ {
			sessions = append(sessions, NewGroupPaneSessionName(sanitized, name))
		}
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		m.message = fmt.Sprintf("Creating session %s from %s...", name, preset.Name)
		return createSessionGroupFromPresetCmd(ref, name, sessions, *preset)
	}

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
	// Keep the dialog open so the user can correct the name in place.
	if err := fleet.ValidateInstanceName(destName); err != nil {
		m.message = err.Error()
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
