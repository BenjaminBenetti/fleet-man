package tui

import (
	"fmt"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	tea "github.com/charmbracelet/bubbletea"
)

// updateCreateSession handles the dialog for creating a new tmux session.
// openCreateSessionDialog opens the new-session dialog for an instance,
// snapshotting the fleet's layout presets for Tab cycling (none selected by
// default — a plain single session).
func (fleetPage *fleetPage) openCreateSessionDialog(m *model, fleetName string, instance *fleet.Instance) tea.Cmd {
	fleetPage.mode = viewCreateSession
	fleetPage.dlg.fleet = fleetName
	fleetPage.dlg.inst = instance.Name
	fleetPage.newSession.presets = nil
	if f, ok := m.st.Fleets[fleetName]; ok {
		fleetPage.newSession.presets = f.Settings.LayoutPresets
	}
	fleetPage.newSession.presetIdx = -1
	fleetPage.textInput.SetValue("")
	fleetPage.textInput.Placeholder = "session-name (or empty for auto)"
	fleetPage.textInput.CharLimit = 64
	return fleetPage.activateTextInput()
}

// cyclePresetTemplate steps the new-session dialog's template selection:
// -1 (none) → preset 0 → … → preset N-1 → back to none, mirroring how the
// add-instance dialog Tab-cycles backends.
func (fleetPage *fleetPage) cyclePresetTemplate(direction int) {
	n := len(fleetPage.newSession.presets)
	if n == 0 {
		return
	}
	// Shift the -1..n-1 range to 0..n and cycle modulo n+1.
	fleetPage.newSession.presetIdx = (fleetPage.newSession.presetIdx+1+direction+n+1)%(n+1) - 1
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

		if fleetPage.dlg.fieldActive {
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
	ref := InstanceRef{Fleet: fleetPage.dlg.fleet, Instance: fleetPage.dlg.inst}

	// Resolve the Tab-selected template (if any) up front so an empty name can
	// default to the template's name rather than the generic "session-N".
	var preset *fleet.LayoutPreset
	if fleetPage.newSession.presetIdx >= 0 && fleetPage.newSession.presetIdx < len(fleetPage.newSession.presets) {
		preset = &fleetPage.newSession.presets[fleetPage.newSession.presetIdx]
	}

	if name == "" {
		if preset != nil {
			name = preset.Name // default to the template's name
		} else {
			name = nextSessionName(m.sessionStore.Sessions(ref))
		}
	}
	name = SanitizeSessionName(name)

	f, ok := m.st.Fleets[fleetPage.dlg.fleet]
	if !ok {
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}
	instance, err := f.GetInstance(fleetPage.dlg.inst)
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
		// Mint the group under a FRESH RANDOM id (like a split group), not the
		// preset name. A preset-derived root name re-collides forever on
		// new-session once that group exists — the reported "stuck, restart
		// doesn't help" bug, since the session lives in the container. A random
		// id never collides on re-apply, and (unlike a readable suffix such as
		// dev-2) can't prefix-match a sibling group in the prefix-based group ops
		// (open/rename/delete). Readable/typed layout-group names need
		// boundary-aware matching first — that's the deferred P1 prefix work.
		groupID := randomHex(3)
		fullName = GroupSessionName(sanitized, groupID)

		sessions := make([]string, 0, preset.PaneCount())
		sessions = append(sessions, fullName)
		for i := 1; i < preset.PaneCount(); i++ {
			sessions = append(sessions, NewGroupPaneSessionName(sanitized, groupID))
		}
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		m.message = fmt.Sprintf("Creating session from %s...", preset.Name)
		return createSessionGroupFromPresetCmd(ref, groupID, sessions, *preset)
	}

	fleetPage.mode = viewNormal
	fleetPage.blurDialogFields()
	m.message = fmt.Sprintf("Creating session %s...", name)
	return createSessionCmd(ref, fullName)
}

// updateRenameSession handles the dialog for renaming a tmux session.
func (fleetPage *fleetPage) updateRenameSession(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if fleetPage.dlg.fieldActive {
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

	ref := InstanceRef{Fleet: fleetPage.dlg.fleet, Instance: fleetPage.dlg.inst}

	f, ok := m.st.Fleets[fleetPage.dlg.fleet]
	if !ok {
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}
	instance, err := f.GetInstance(fleetPage.dlg.inst)
	if err != nil {
		fleetPage.mode = viewNormal
		fleetPage.blurDialogFields()
		return nil
	}

	sanitized := SanitizeSessionName(instance.Name)
	oldName := fleetPage.dlg.session
	oldGID, isGrouped := parseGroupID(sanitized, oldName)

	fleetPage.mode = viewNormal
	fleetPage.blurDialogFields()

	if isGrouped {
		return renameGroupCmd(ref, sanitized, oldGID, newName)
	}
	return renameSessionCmd(ref, oldName, newName)
}

// createSessionState holds the new-session dialog's preset templating: the
// fleet's presets snapshotted at dialog open and the Tab-cycled selection
// (-1 = none, a plain session).
type createSessionState struct {
	presets   []fleet.LayoutPreset
	presetIdx int
}

func (fleetPage *fleetPage) renderCreateSessionDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	hint := fleetPage.textDialogHint("Create (empty for auto-name)")
	body := fmt.Sprintf(
		"%s\n\n%s %s\n%s %s",
		dialogTitle.Render("New session"),
		dialogLabel.Render("Instance:"),
		fleetExpandedStyle.Render(fleetPage.dlg.fleet+"/"+fleetPage.dlg.inst),
		dialogLabel.Render("Name:    "),
		fleetPage.textInput.View(),
	)
	// Template line, only when the fleet has layout presets to cycle. Shown
	// as a bracketed cycle option ([ none ] / [ name ]) like the backend
	// selector, with the chosen template highlighted.
	if len(fleetPage.newSession.presets) > 0 {
		var tmpl string
		if idx := fleetPage.newSession.presetIdx; idx >= 0 && idx < len(fleetPage.newSession.presets) {
			tmpl = selectedStyle.Render(fmt.Sprintf("[ %s ]", fleetPage.newSession.presets[idx].Name))
		} else {
			tmpl = dimStyle.Render("[ none ]")
		}
		body += fmt.Sprintf("\n%s %s", dialogLabel.Render("Template:"), tmpl)
		hint = "[tab] Cycle template  " + hint
	}
	dialog := fmt.Sprintf("%s\n\n%s", body, dialogHint.Render(hint))
	b.WriteString(dialogBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}

func (fleetPage *fleetPage) renderRenameSessionDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	dialog := fmt.Sprintf(
		"%s\n\n%s %s\n%s %s\n%s %s\n\n%s",
		dialogTitle.Render("Rename session"),
		dialogLabel.Render("Instance:"),
		fleetExpandedStyle.Render(fleetPage.dlg.fleet+"/"+fleetPage.dlg.inst),
		dialogLabel.Render("Current: "),
		sessionStyle.Render(fleetPage.dlg.session),
		dialogLabel.Render("New:     "),
		fleetPage.textInput.View(),
		dialogHint.Render(fleetPage.textDialogHint("Rename")),
	)
	b.WriteString(dialogBox.Render(dialog))
	b.WriteString("\n")

	return b.String()
}
