package tui

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	tea "github.com/charmbracelet/bubbletea"
)

// ===========================================
// Delete Dialogs
// ===========================================

// updateConfirmDelete handles the instance/fleet deletion confirmation dialog.
func (fp *fleetPage) updateConfirmDelete(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y", "enter":
			if fp.dialogInst == "" {
				// Fleet-level delete — check if it has instances for double confirm
				if f, ok := m.st.Fleets[fp.dialogFleet]; ok && len(f.Instances) > 0 {
					fp.mode = viewConfirmDeleteFleetWarn
					return nil
				}
				// Empty fleet, just remove it
				delete(m.st.Fleets, fp.dialogFleet)
				delete(fp.collapsed, fp.dialogFleet)
				_ = state.Save(m.st)
				fp.buildRows(m)
				m.message = fmt.Sprintf("Removed fleet %s", fp.dialogFleet)
			} else {
				// Instance-level delete (async with transitional status)
				f, ok := m.st.Fleets[fp.dialogFleet]
				if ok {
					inst, err := f.GetInstance(fp.dialogInst)
					if err == nil {
						inst.Status = fleet.StatusDeleting
						_ = state.Save(m.st)
						fp.buildRows(m)
						fp.mode = viewNormal
						dc := m.instanceBackend(inst)
						return deleteInstanceCmd(dc, fp.dialogFleet, fp.dialogInst, inst.ContainerID, inst.WorkspaceDir, m.portForwards)
					}
				}
			}
			fp.mode = viewNormal

		case "n", "N", "esc", "ctrl+c":
			fp.mode = viewNormal
			m.message = "Cancelled"
		}
	}
	return nil
}

// updateConfirmDeleteFleetWarn handles the double-confirm dialog for
// fleets with running instances.
func (fp *fleetPage) updateConfirmDeleteFleetWarn(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y", "enter":
			f, ok := m.st.Fleets[fp.dialogFleet]
			if ok && len(f.Instances) > 0 {
				for _, inst := range f.Instances {
					inst.Status = fleet.StatusDeleting
				}
				_ = state.Save(m.st)
				fp.buildRows(m)
				fp.mode = viewNormal
				return deleteFleetCmd(m.backends, fp.dialogFleet, f.Instances, m.portForwards)
			} else if ok {
				delete(m.st.Fleets, fp.dialogFleet)
				delete(fp.collapsed, fp.dialogFleet)
				_ = state.Save(m.st)
				fp.buildRows(m)
				m.message = fmt.Sprintf("Removed fleet %s", fp.dialogFleet)
			}
			fp.mode = viewNormal

		case "n", "N", "esc", "ctrl+c":
			fp.mode = viewNormal
			m.message = "Cancelled"
		}
	}
	return nil
}

// updateConfirmDeleteSession handles the session deletion confirmation dialog.
func (fp *fleetPage) updateConfirmDeleteSession(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "y", "Y", "enter":
			fp.mode = viewNormal
			instKey := fp.dialogFleet + "/" + fp.dialogInst
			f, ok := m.st.Fleets[fp.dialogFleet]
			if !ok {
				break
			}
			inst, err := f.GetInstance(fp.dialogInst)
			if err != nil {
				break
			}
			b := m.instanceBackend(inst)
			sanitized := SanitizeSessionName(inst.Name)
			if fp.dialogGroupID != "" && isGroupedSession(sanitized, fp.dialogSession) {
				return deleteGroupSessionsCmd(b, inst.WorkspaceDir, instKey, sanitized, fp.dialogGroupID)
			}
			return deleteSessionCmd(b, inst.WorkspaceDir, instKey, fp.dialogSession)

		case "n", "N", "esc", "ctrl+c":
			fp.mode = viewNormal
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
	fleet.BackendLocalDir:     "",
}

// allBackendTypes is the ordered master list of every backend type.
var allBackendTypes = []fleet.BackendType{
	fleet.BackendDevcontainer,
	fleet.BackendCoder,
	fleet.BackendCodespaces,
	fleet.BackendLocalDir,
}

// nextBackendType cycles through the given options list from current.
func nextBackendType(current fleet.BackendType, direction int, options []fleet.BackendType) fleet.BackendType {
	if len(options) == 0 {
		return current
	}
	idx := 0
	for i, bt := range options {
		if bt == current {
			idx = i
			break
		}
	}
	idx = (idx + direction + len(options)) % len(options)
	return options[idx]
}

// backendTypeLabel returns a human-readable label for a backend type.
func backendTypeLabel(bt fleet.BackendType) string {
	switch bt {
	case fleet.BackendCoder:
		return "Coder"
	case fleet.BackendCodespaces:
		return "Codespaces"
	case fleet.BackendLocalDir:
		return "Local Dir"
	default:
		return "Devcontainer"
	}
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
func (fp *fleetPage) openEditInstanceDialog(m *model) tea.Cmd {
	f, inst := fp.selectedInstance(m)
	if inst == nil || f == nil {
		m.message = "Select an instance to edit"
		return nil
	}

	fp.mode = viewAddInstance
	fp.dialogEditing = true
	fp.dialogFleet = f.Name
	fp.dialogInst = inst.Name
	fp.dialogBackend = inst.Backend
	if fp.dialogBackend == "" {
		fp.dialogBackend = fleet.BackendDevcontainer
	}
	fp.dialogColor = inst.Color
	if fp.dialogColor == "" {
		fp.dialogColor = instanceColorWhite
	}
	fp.dialogRow = addInstanceRowName
	fp.textInput.SetValue(inst.GetDisplayName())
	fp.branchInput.SetValue(inst.Branch)
	fp.syncAddInstanceFocus()
	return fp.textInput.Cursor.BlinkCmd()
}

// updateAddInstance handles the add-instance dialog.
func (fp *fleetPage) updateAddInstance(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			if fp.dialogEditing {
				return fp.saveInstanceEdits(m)
			}
			name := strings.TrimSpace(fp.textInput.Value())
			if name == "" {
				m.message = "Name cannot be empty"
				fp.mode = viewNormal
				return nil
			}

			fleetName := fp.dialogFleet
			f, ok := m.st.Fleets[fleetName]
			if !ok {
				m.message = fmt.Sprintf("Fleet %s not found", fleetName)
				fp.mode = viewNormal
				return nil
			}

			if _, err := f.GetInstance(name); err == nil {
				m.message = fmt.Sprintf("Instance %s/%s already exists", fleetName, name)
				fp.mode = viewNormal
				return nil
			}

			bt := fp.dialogBackend
			if bt == "" {
				bt = fleet.BackendDevcontainer
			}

			color := fp.dialogColor
			if color == instanceColorWhite {
				color = ""
			}

			branch := strings.TrimSpace(fp.branchInput.Value())

			wsDir := filepath.Join(state.WorkspacesDir(), fleetName, name, fleetName)
			inst := &fleet.Instance{
				Name:         name,
				DisplayName:  name,
				Config:       ".devcontainer/devcontainer.json",
				WorkspaceDir: wsDir,
				CreatedAt:    time.Now(),
				Status:       fleet.StatusCreating,
				Backend:      bt,
				Color:        color,
				Branch:       branch,
			}
			_ = f.AddInstance(inst)
			_ = state.Save(m.st)

			if m.cfg != nil {
				m.cfg.DefaultBackend = string(bt)
				_ = state.SaveConfig(m.cfg)
			}

			key := fleetName + "/" + name
			m.creating[key] = true
			fp.buildRows(m)
			fp.mode = viewNormal
			fp.textInput.Blur()
			fp.branchInput.Blur()
			m.message = fmt.Sprintf("Creating %s (%s)...", key, backendTypeLabel(bt))

			return createInstanceCmd(fleetName, name, f.Remote, branch, bt)

		case "tab":
			if fp.dialogEditing {
				return nil
			}
			opts := fp.availableBackendTypes(m)
			if len(opts) > 1 {
				fp.dialogBackend = nextBackendType(fp.dialogBackend, 1, opts)
			}
			return nil

		case "shift+tab":
			fp.dialogColor = nextInstanceColor(fp.dialogColor, 1)
			return nil

		case "up":
			fp.dialogRow = fp.prevAddInstanceRow(fp.dialogRow)
			fp.syncAddInstanceFocus()
			return nil

		case "down":
			fp.dialogRow = fp.nextAddInstanceRow(fp.dialogRow)
			fp.syncAddInstanceFocus()
			return nil

		case "left":
			if fp.dialogRow == addInstanceRowDeploy && !fp.dialogEditing {
				opts := fp.availableBackendTypes(m)
				if len(opts) > 1 {
					fp.dialogBackend = nextBackendType(fp.dialogBackend, -1, opts)
				}
				return nil
			}
			if fp.dialogRow == addInstanceRowColor {
				fp.dialogColor = nextInstanceColor(fp.dialogColor, -1)
				return nil
			}

		case "right", " ":
			if fp.dialogRow == addInstanceRowDeploy && !fp.dialogEditing {
				opts := fp.availableBackendTypes(m)
				if len(opts) > 1 {
					fp.dialogBackend = nextBackendType(fp.dialogBackend, 1, opts)
				}
				return nil
			}
			if fp.dialogRow == addInstanceRowColor {
				fp.dialogColor = nextInstanceColor(fp.dialogColor, 1)
				return nil
			}

		case "esc", "ctrl+c":
			fp.mode = viewNormal
			fp.textInput.Blur()
			fp.branchInput.Blur()
			m.message = "Cancelled"
			return nil
		}
	}

	// Forward key input to whichever text field currently has focus. The
	// color and deploy rows are cycled with arrow keys and should swallow
	// character input rather than mutate either field.
	switch fp.dialogRow {
	case addInstanceRowName:
		var cmd tea.Cmd
		fp.textInput, cmd = fp.textInput.Update(msg)
		return cmd
	case addInstanceRowBranch:
		var cmd tea.Cmd
		fp.branchInput, cmd = fp.branchInput.Update(msg)
		return cmd
	}
	return nil
}

// syncAddInstanceFocus focuses the text input of the currently selected
// row so the cursor visually reflects the current focus. In edit mode
// the branch input is immutable so it never receives focus; the name
// input edits DisplayName and stays focusable.
func (fp *fleetPage) syncAddInstanceFocus() {
	nameFocus := fp.dialogRow == addInstanceRowName
	branchFocus := fp.dialogRow == addInstanceRowBranch && !fp.dialogEditing

	if nameFocus {
		fp.textInput.Focus()
	} else {
		fp.textInput.Blur()
	}

	if branchFocus {
		fp.branchInput.Focus()
	} else {
		fp.branchInput.Blur()
	}
}

// addInstanceRowEnabled reports whether a given row is selectable in the
// current dialog mode. Branch and deploy are locked while editing because
// they describe how the workspace was originally provisioned and cannot
// be retroactively changed without recreating the instance.
func (fp *fleetPage) addInstanceRowEnabled(row int) bool {
	if !fp.dialogEditing {
		return true
	}
	return row == addInstanceRowName || row == addInstanceRowColor
}

// nextAddInstanceRow advances the focused row forward, skipping any rows
// that are disabled in the current dialog mode.
func (fp *fleetPage) nextAddInstanceRow(current int) int {
	for i := 1; i <= addInstanceRowCount; i++ {
		candidate := (current + i) % addInstanceRowCount
		if fp.addInstanceRowEnabled(candidate) {
			return candidate
		}
	}
	return current
}

// prevAddInstanceRow advances the focused row backward, skipping any rows
// that are disabled in the current dialog mode.
func (fp *fleetPage) prevAddInstanceRow(current int) int {
	for i := 1; i <= addInstanceRowCount; i++ {
		candidate := (current - i + addInstanceRowCount) % addInstanceRowCount
		if fp.addInstanceRowEnabled(candidate) {
			return candidate
		}
	}
	return current
}

// saveInstanceEdits commits display-name and color edits to the selected
// instance and closes the dialog. The underlying Name is immutable; the
// name input writes to DisplayName instead.
func (fp *fleetPage) saveInstanceEdits(m *model) tea.Cmd {
	f, ok := m.st.Fleets[fp.dialogFleet]
	if !ok {
		fp.mode = viewNormal
		fp.dialogEditing = false
		m.message = fmt.Sprintf("Fleet %s not found", fp.dialogFleet)
		return nil
	}
	inst, err := f.GetInstance(fp.dialogInst)
	if err != nil {
		fp.mode = viewNormal
		fp.dialogEditing = false
		m.message = fmt.Sprintf("Instance %s/%s not found", fp.dialogFleet, fp.dialogInst)
		return nil
	}

	displayName := strings.TrimSpace(fp.textInput.Value())
	if displayName == "" {
		m.message = "Name cannot be empty"
		return nil
	}

	color := fp.dialogColor
	if color == instanceColorWhite {
		color = ""
	}
	inst.DisplayName = displayName
	inst.Color = color
	_ = state.Save(m.st)

	fp.buildRows(m)
	fp.mode = viewNormal
	fp.dialogEditing = false
	fp.textInput.Blur()
	m.message = fmt.Sprintf("Updated %s/%s", fp.dialogFleet, fp.dialogInst)
	return nil
}

// ===========================================
// Tag Instance Dialog
// ===========================================

// updateTagInstance handles the tag-instance dialog.
func (fp *fleetPage) updateTagInstance(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			tag := strings.TrimSpace(fp.textInput.Value())

			f, ok := m.st.Fleets[fp.dialogFleet]
			if ok {
				if inst, err := f.GetInstance(fp.dialogInst); err == nil {
					inst.Tag = tag
					_ = state.Save(m.st)
				}
			}

			fp.mode = viewNormal
			fp.textInput.Blur()
			if tag == "" {
				m.message = fmt.Sprintf("Cleared tag for %s/%s", fp.dialogFleet, fp.dialogInst)
			} else {
				m.message = fmt.Sprintf("Tagged %s/%s: %s", fp.dialogFleet, fp.dialogInst, tag)
			}
			return nil

		case "esc", "ctrl+c":
			fp.mode = viewNormal
			fp.textInput.Blur()
			m.message = "Cancelled"
			return nil
		}
	}

	var cmd tea.Cmd
	fp.textInput, cmd = fp.textInput.Update(msg)
	return cmd
}

// ===========================================
// Add Fleet Dialog
// ===========================================

// updateAddFleet handles the add-fleet dialog.
func (fp *fleetPage) updateAddFleet(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			repoURL := strings.TrimSpace(fp.textInput.Value())
			if repoURL == "" {
				m.message = "URL cannot be empty"
				fp.mode = viewNormal
				return nil
			}
			fleetName := fleet.FleetNameFromRemote(repoURL)
			if fleetName == "" {
				m.message = "Could not derive fleet name from URL"
				fp.mode = viewNormal
				return nil
			}
			m.st.GetOrCreateFleet(fleetName, repoURL)
			_ = state.Save(m.st)
			fp.buildRows(m)
			fp.mode = viewNormal
			fp.textInput.Blur()
			m.message = fmt.Sprintf("Added fleet %s", fleetName)
			return nil

		case "esc", "ctrl+c":
			fp.mode = viewNormal
			fp.textInput.Blur()
			m.message = "Cancelled"
			return nil
		}
	}

	var cmd tea.Cmd
	fp.textInput, cmd = fp.textInput.Update(msg)
	return cmd
}

// ===========================================
// Port Forward Dialog
// ===========================================

// updatePortForward handles the port forward management dialog.
func (fp *fleetPage) updatePortForward(m *model, msg tea.Msg) tea.Cmd {
	key := fp.dialogFleet + "/" + fp.dialogInst

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			raw := strings.TrimSpace(fp.textInput.Value())
			if raw == "" {
				return nil
			}
			local, remote, err := parsePortMapping(raw)
			if err != nil {
				m.message = err.Error()
				return nil
			}

			b := m.instanceBackend(&fleet.Instance{Backend: fp.instanceBackendType(m)})
			if err := m.portForwards.Add(key, local, remote, b.PortForwardCommand, fp.pfContainerID, b.ResolveHostname); err != nil {
				m.message = err.Error()
				return nil
			}

			fp.textInput.SetValue("")
			m.message = fmt.Sprintf("Forwarding localhost:%d -> %s:%d", local, fp.dialogInst, remote)
			return nil

		case "d":
			fwds := m.portForwards.List(key)
			if len(fwds) > 0 && fp.pfCursor < len(fwds) {
				fwd := fwds[fp.pfCursor]
				_ = m.portForwards.Remove(key, fwd.LocalPort)
				m.message = fmt.Sprintf("Removed forward %s", fwd.Label())
				if fp.pfCursor >= len(m.portForwards.List(key)) {
					fp.pfCursor = max(0, fp.pfCursor-1)
				}
				return nil
			}

		case "up", "k":
			if fp.pfCursor > 0 {
				fp.pfCursor--
			}
			return nil

		case "down", "j":
			fwds := m.portForwards.List(key)
			if fp.pfCursor < len(fwds)-1 {
				fp.pfCursor++
			}
			return nil

		case "esc", "ctrl+c":
			fp.mode = viewNormal
			fp.textInput.Blur()
			fwds := m.portForwards.List(key)
			if len(fwds) > 0 {
				m.message = fmt.Sprintf("%d port forward(s) active on %s", len(fwds), fp.dialogInst)
			} else {
				m.message = ""
			}
			return nil
		}
	}

	var cmd tea.Cmd
	fp.textInput, cmd = fp.textInput.Update(msg)
	return cmd
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
func (fp *fleetPage) updateCodespacesAuth(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			fp.mode = viewNormal
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
		case "esc", "ctrl+c":
			fp.mode = viewNormal
			m.message = "Auth cancelled — codespace creation requires the codespace scope"
		}
	}
	return nil
}

// updateCodespacesMachine handles the dialog shown when gh needs a
// machine type but none is configured.
func (fp *fleetPage) updateCodespacesMachine(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			fp.mode = viewNormal
			m.message = "Set the Machine field, then retry instance creation"
			return m.ChangeRoute(routeSettings)
		case "esc", "ctrl+c":
			fp.mode = viewNormal
			m.message = ""
		}
	}
	return nil
}

// updateCodespacesLimit handles the dialog shown when the user has
// hit the maximum codespace count.
func (fp *fleetPage) updateCodespacesLimit(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter", "esc", "ctrl+c":
			fp.mode = viewNormal
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
func (fp *fleetPage) instanceBackendType(m *model) fleet.BackendType {
	if f, ok := m.st.Fleets[fp.dialogFleet]; ok {
		if inst, err := f.GetInstance(fp.dialogInst); err == nil {
			return inst.Backend
		}
	}
	return fleet.BackendDevcontainer
}

// ===========================================
// Session Dialogs
// ===========================================

// updateCreateSession handles the dialog for creating a new tmux session.
func (fp *fleetPage) updateCreateSession(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			name := strings.TrimSpace(fp.textInput.Value())
			instKey := fp.dialogFleet + "/" + fp.dialogInst
			if name == "" {
				var existing []tmuxSession
				if disc, ok := m.sessions[instKey]; ok && disc.err == nil {
					existing = disc.sessions
				}
				name = nextSessionName(existing)
			}
			name = SanitizeSessionName(name)

			f, ok := m.st.Fleets[fp.dialogFleet]
			if !ok {
				fp.mode = viewNormal
				fp.textInput.Blur()
				return nil
			}
			inst, err := f.GetInstance(fp.dialogInst)
			if err != nil {
				fp.mode = viewNormal
				fp.textInput.Blur()
				return nil
			}

			sanitized := SanitizeSessionName(inst.Name)
			fullName := groupSessionName(sanitized, name)

			fp.mode = viewNormal
			fp.textInput.Blur()
			m.message = fmt.Sprintf("Creating session %s...", name)
			b := m.instanceBackend(inst)
			return createSessionCmd(b, inst.WorkspaceDir, instKey, fullName)

		case "esc", "ctrl+c":
			fp.mode = viewNormal
			fp.textInput.Blur()
			m.message = "Cancelled"
			return nil
		}
	}

	var cmd tea.Cmd
	fp.textInput, cmd = fp.textInput.Update(msg)
	return cmd
}

// updateRenameSession handles the dialog for renaming a tmux session.
func (fp *fleetPage) updateRenameSession(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			newName := strings.TrimSpace(fp.textInput.Value())
			if newName == "" {
				m.message = "Name cannot be empty"
				fp.mode = viewNormal
				fp.textInput.Blur()
				return nil
			}
			newName = SanitizeSessionName(newName)

			instKey := fp.dialogFleet + "/" + fp.dialogInst

			f, ok := m.st.Fleets[fp.dialogFleet]
			if !ok {
				fp.mode = viewNormal
				fp.textInput.Blur()
				return nil
			}
			inst, err := f.GetInstance(fp.dialogInst)
			if err != nil {
				fp.mode = viewNormal
				fp.textInput.Blur()
				return nil
			}

			sanitized := SanitizeSessionName(inst.Name)
			oldName := fp.dialogSession
			oldGID, isGrouped := parseGroupID(sanitized, oldName)

			fp.mode = viewNormal
			fp.textInput.Blur()
			b := m.instanceBackend(inst)

			if isGrouped {
				return renameGroupCmd(b, inst.WorkspaceDir, instKey, sanitized, oldGID, newName)
			}
			return renameSessionCmd(b, inst.WorkspaceDir, instKey, oldName, newName)

		case "esc", "ctrl+c":
			fp.mode = viewNormal
			fp.textInput.Blur()
			m.message = "Cancelled"
			return nil
		}
	}

	var cmd tea.Cmd
	fp.textInput, cmd = fp.textInput.Update(msg)
	return cmd
}
