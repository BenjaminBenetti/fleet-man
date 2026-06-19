package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	tea "github.com/charmbracelet/bubbletea"
)

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
	fleetPage.addInst.editing = true
	fleetPage.dlg.fleet = f.Name
	fleetPage.dlg.inst = instance.Name
	fleetPage.addInst.backend = instance.Backend
	if fleetPage.addInst.backend == "" {
		fleetPage.addInst.backend = fleet.BackendDevcontainer
	}
	fleetPage.addInst.color = instance.Color
	if fleetPage.addInst.color == "" {
		fleetPage.addInst.color = instanceColorWhite
	}
	fleetPage.dlg.row = addInstanceRowName
	fleetPage.dlg.fieldActive = false
	fleetPage.textInput.SetValue(instance.GetDisplayName())
	fleetPage.branchInput.SetValue(instance.Branch)
	fleetPage.syncAddInstanceFocus()
	return nil
}

// updateAddInstance handles the add-instance dialog.
func (fleetPage *fleetPage) updateAddInstance(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dlg.fieldActive {
			switch msg.String() {
			case "enter":
				return fleetPage.submitAddInstance(m)
			case "esc":
				fleetPage.dlg.fieldActive = false
				fleetPage.syncAddInstanceFocus()
				return nil
			case "ctrl+c":
				return fleetPage.cancelAddInstance(m)
			}
			return fleetPage.updateActiveAddInstanceField(msg)
		}

		switch msg.String() {
		case "enter":
			if fleetPage.dlg.row == addInstanceRowName || (fleetPage.dlg.row == addInstanceRowBranch && !fleetPage.addInst.editing) {
				return fleetPage.activateAddInstanceField()
			}
			return fleetPage.submitAddInstance(m)

		case "tab":
			if fleetPage.addInst.editing {
				return nil
			}
			opts := fleetPage.availableBackendTypes(m)
			if len(opts) > 1 {
				fleetPage.addInst.backend = nextBackendType(fleetPage.addInst.backend, 1, opts)
			}
			return nil

		case "shift+tab":
			fleetPage.addInst.color = nextInstanceColor(fleetPage.addInst.color, 1)
			return nil

		case "up":
			fleetPage.dlg.fieldActive = false
			fleetPage.dlg.row = fleetPage.prevAddInstanceRow(fleetPage.dlg.row)
			fleetPage.syncAddInstanceFocus()
			return nil

		case "down":
			fleetPage.dlg.fieldActive = false
			fleetPage.dlg.row = fleetPage.nextAddInstanceRow(fleetPage.dlg.row)
			fleetPage.syncAddInstanceFocus()
			return nil

		case "left":
			if fleetPage.dlg.row == addInstanceRowDeploy && !fleetPage.addInst.editing {
				opts := fleetPage.availableBackendTypes(m)
				if len(opts) > 1 {
					fleetPage.addInst.backend = nextBackendType(fleetPage.addInst.backend, -1, opts)
				}
				return nil
			}
			if fleetPage.dlg.row == addInstanceRowColor {
				fleetPage.addInst.color = nextInstanceColor(fleetPage.addInst.color, -1)
				return nil
			}

		case "right", " ":
			if msg.String() == " " && (fleetPage.dlg.row == addInstanceRowName || (fleetPage.dlg.row == addInstanceRowBranch && !fleetPage.addInst.editing)) {
				return fleetPage.activateAddInstanceField()
			}
			if fleetPage.dlg.row == addInstanceRowDeploy && !fleetPage.addInst.editing {
				opts := fleetPage.availableBackendTypes(m)
				if len(opts) > 1 {
					fleetPage.addInst.backend = nextBackendType(fleetPage.addInst.backend, 1, opts)
				}
				return nil
			}
			if fleetPage.dlg.row == addInstanceRowColor {
				fleetPage.addInst.color = nextInstanceColor(fleetPage.addInst.color, 1)
				return nil
			}

		case "esc", "q", "Q", "ctrl+c":
			return fleetPage.cancelAddInstance(m)
		}

		if isDialogUpKey(msg.String()) {
			fleetPage.dlg.fieldActive = false
			fleetPage.dlg.row = fleetPage.prevAddInstanceRow(fleetPage.dlg.row)
			fleetPage.syncAddInstanceFocus()
			return nil
		}
		if isDialogDownKey(msg.String()) {
			fleetPage.dlg.fieldActive = false
			fleetPage.dlg.row = fleetPage.nextAddInstanceRow(fleetPage.dlg.row)
			fleetPage.syncAddInstanceFocus()
			return nil
		}
		if isDialogLeftKey(msg.String()) {
			if fleetPage.dlg.row == addInstanceRowDeploy && !fleetPage.addInst.editing {
				opts := fleetPage.availableBackendTypes(m)
				if len(opts) > 1 {
					fleetPage.addInst.backend = nextBackendType(fleetPage.addInst.backend, -1, opts)
				}
				return nil
			}
			if fleetPage.dlg.row == addInstanceRowColor {
				fleetPage.addInst.color = nextInstanceColor(fleetPage.addInst.color, -1)
				return nil
			}
		}
		if isDialogRightKey(msg.String()) {
			if fleetPage.dlg.row == addInstanceRowDeploy && !fleetPage.addInst.editing {
				opts := fleetPage.availableBackendTypes(m)
				if len(opts) > 1 {
					fleetPage.addInst.backend = nextBackendType(fleetPage.addInst.backend, 1, opts)
				}
				return nil
			}
			if fleetPage.dlg.row == addInstanceRowColor {
				fleetPage.addInst.color = nextInstanceColor(fleetPage.addInst.color, 1)
				return nil
			}
		}
		if isDialogTextKey(msg) && (fleetPage.dlg.row == addInstanceRowName || (fleetPage.dlg.row == addInstanceRowBranch && !fleetPage.addInst.editing)) {
			return fleetPage.activateAddInstanceFieldWithMsg(msg)
		}
	}

	return nil
}

func (fleetPage *fleetPage) updateActiveAddInstanceField(msg tea.Msg) tea.Cmd {
	switch fleetPage.dlg.row {
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
	if fleetPage.addInst.editing {
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

	fleetName := fleetPage.dlg.fleet
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

	backendType := fleetPage.addInst.backend
	if backendType == "" {
		backendType = fleet.BackendDevcontainer
	}

	color := fleetPage.addInst.color
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
	fleetPage.addInst.editing = false
	fleetPage.blurDialogFields()
	m.message = "Cancelled"
	return nil
}

// syncAddInstanceFocus focuses the text input of the currently selected
// row so the cursor visually reflects the current focus. In edit mode
// the branch input is immutable so it never receives focus; the name
// input edits DisplayName and stays focusable.
func (fleetPage *fleetPage) syncAddInstanceFocus() {
	nameFocus := fleetPage.dlg.fieldActive && fleetPage.dlg.row == addInstanceRowName
	branchFocus := fleetPage.dlg.fieldActive && fleetPage.dlg.row == addInstanceRowBranch && !fleetPage.addInst.editing

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
	fleetPage.dlg.fieldActive = true
	fleetPage.syncAddInstanceFocus()
	switch fleetPage.dlg.row {
	case addInstanceRowName:
		return fleetPage.textInput.Cursor.BlinkCmd()
	case addInstanceRowBranch:
		if !fleetPage.addInst.editing {
			return fleetPage.branchInput.Cursor.BlinkCmd()
		}
	}
	fleetPage.dlg.fieldActive = false
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
	if !fleetPage.addInst.editing {
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
	f, ok := m.st.Fleets[fleetPage.dlg.fleet]
	if !ok {
		fleetPage.mode = viewNormal
		fleetPage.addInst.editing = false
		fleetPage.blurDialogFields()
		m.message = fmt.Sprintf("Fleet %s not found", fleetPage.dlg.fleet)
		return nil
	}
	instance, err := f.GetInstance(fleetPage.dlg.inst)
	if err != nil {
		fleetPage.mode = viewNormal
		fleetPage.addInst.editing = false
		fleetPage.blurDialogFields()
		m.message = fmt.Sprintf("Instance %s/%s not found", fleetPage.dlg.fleet, fleetPage.dlg.inst)
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

	color := fleetPage.addInst.color
	if color == instanceColorWhite {
		color = ""
	}
	instance.DisplayName = displayName
	instance.Color = color
	_ = setInstanceMetadataRemote(fleetPage.dlg.fleet, fleetPage.dlg.inst, &displayName, &color, nil)

	fleetPage.buildRows(m)
	fleetPage.mode = viewNormal
	fleetPage.addInst.editing = false
	fleetPage.blurDialogFields()
	m.message = fmt.Sprintf("Updated %s/%s", fleetPage.dlg.fleet, fleetPage.dlg.inst)
	return nil
}

// ===========================================
// Tag Instance Dialog
// ===========================================

// updateTagInstance handles the tag-instance dialog.
func (fleetPage *fleetPage) updateTagInstance(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dlg.fieldActive {
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

	f, ok := m.st.Fleets[fleetPage.dlg.fleet]
	if ok {
		if instance, err := f.GetInstance(fleetPage.dlg.inst); err == nil {
			instance.Tag = tag
			_ = setInstanceMetadataRemote(fleetPage.dlg.fleet, fleetPage.dlg.inst, nil, nil, &tag)
		}
	}

	fleetPage.mode = viewNormal
	fleetPage.blurDialogFields()
	// The tag renders as its own row under an expanded instance, so the
	// row list must be rebuilt for the change to show immediately.
	fleetPage.buildRows(m)
	if tag == "" {
		m.message = fmt.Sprintf("Cleared tag for %s/%s", fleetPage.dlg.fleet, fleetPage.dlg.inst)
	} else {
		m.message = fmt.Sprintf("Tagged %s/%s: %s", fleetPage.dlg.fleet, fleetPage.dlg.inst, tag)
	}
	return nil
}

// updateCloneInstance handles the single-text-input dialog that asks
// the user for a destination instance name when cloning. dlg.fleet
// and dlg.inst hold the source instance's identifiers.
func (fleetPage *fleetPage) updateCloneInstance(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dlg.fieldActive {
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

	fleetName := fleetPage.dlg.fleet
	srcName := fleetPage.dlg.inst

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

// addInstanceState holds the add/edit-instance form's non-shared fields (the
// target fleet/instance live in fleetPage.dlg).
type addInstanceState struct {
	backend fleet.BackendType
	color   string
	editing bool
}

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

func (fleetPage *fleetPage) renderAddInstanceDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	backendType := fleetPage.addInst.backend
	if backendType == "" {
		backendType = fleet.BackendDevcontainer
	}
	colorName := fleetPage.addInst.color
	if colorName == "" {
		colorName = instanceColorWhite
	}

	var title, hint, nameField, branchField, deployField string
	if fleetPage.addInst.editing {
		title = "Edit instance"
		if fleetPage.dlg.fieldActive {
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
		if fleetPage.dlg.fieldActive {
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
		if fleetPage.dlg.row == r {
			return cursorStyle.Render("> ")
		}
		return "  "
	}

	colorPreview := instanceColorStyle(colorName).Render(colorName)
	dialog := fmt.Sprintf(
		"%s\n\n  %s %s\n%s%s %s\n%s%s %s\n%s%s [ %s ]\n%s%s %s\n\n%s",
		dialogTitle.Render(title),
		dialogLabel.Render("Fleet:  "),
		fleetExpandedStyle.Render(fleetPage.dlg.fleet),
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

	return b.String()
}

func (fleetPage *fleetPage) renderTagInstanceDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	dialog := fmt.Sprintf(
		"%s\n\n%s %s\n%s %s\n\n%s",
		dialogTitle.Render("Tag instance"),
		dialogLabel.Render("Instance:"),
		fleetExpandedStyle.Render(fleetPage.dlg.fleet+"/"+fleetPage.dlg.inst),
		dialogLabel.Render("Tag:     "),
		fleetPage.textInput.View(),
		dialogHint.Render(fleetPage.textDialogHint("Save")),
	)
	b.WriteString(dialogBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

func (fleetPage *fleetPage) renderCloneInstanceDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	dialog := fmt.Sprintf(
		"%s\n\n%s %s\n%s %s\n\n%s",
		dialogTitle.Render("Clone instance"),
		dialogLabel.Render("Source:     "),
		fleetExpandedStyle.Render(fleetPage.dlg.fleet+"/"+fleetPage.dlg.inst),
		dialogLabel.Render("Destination:"),
		fleetPage.textInput.View(),
		dialogHint.Render(fleetPage.textDialogHint("Clone")),
	)
	b.WriteString(dialogBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}
