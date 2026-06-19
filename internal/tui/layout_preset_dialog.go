package tui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// layout_preset_dialog.go implements the layout-preset creation/edit flow
// (issue #150), opened from the edit-fleet dialog's Layouts section. Creation
// is two stages: pick a live session group to capture (its saved outer-tmux
// layout is the template geometry), then in a navigable preview optionally set
// a startup command per pane (an empty one is a plain shell), name the preset,
// and save. Editing an existing preset reuses stage two with its commands
// pre-filled.

type layoutPresetStage int

const (
	lpStagePick layoutPresetStage = iota // choosing the source session group
	lpStageEdit                          // preview + per-pane commands + name
)

// Focus stops in the edit stage, in navigation order: the name row, one stop
// per session pane (slot order = position order), then the buttons.
const (
	lpFocusName = 0
	// pane slots occupy 1..paneCount; the buttons follow.
)

// lpCandidate is one capturable session group shown in the pick stage.
type lpCandidate struct {
	instance  string // instance Name (stable identifier)
	display   string // "<instance display> / <group>" per the issue spec
	groupName string // the group's display name; seeds the preset name
	layout    string // saved outer-tmux layout ("" when never captured)
	paneCount int
}

// layoutPresetFlow holds all state for one open preset flow. It lives on
// fleetPage only while mode == viewLayoutPreset and is rebuilt on every open.
type layoutPresetFlow struct {
	stage   layoutPresetStage
	editIdx int // index into editFleet.layoutPresets being edited; -1 = creating

	// Pick stage.
	candidates []lpCandidate
	pickCursor int

	// Edit stage.
	layout         string       // tmux layout string the preset will carry
	rects          []layoutRect // parsed leaves; rects[0] is the TUI pane
	order          []int        // rect index per slot, position-sorted
	commands       []string     // per-slot startup command ("" = plain shell)
	focus          int
	editingName    bool
	nameBeforeEdit string // restored when a name edit is escaped
	editingCmd     bool
	nameInput      textinput.Model
	cmdInput       textinput.Model
	errMsg         string // inline validation error
}

func (lp *layoutPresetFlow) paneCount() int { return len(lp.order) }

func (lp *layoutPresetFlow) focusCancel() int  { return 1 + lp.paneCount() }
func (lp *layoutPresetFlow) focusConfirm() int { return 2 + lp.paneCount() }

// focusedSlot returns the pane slot the focus is on, or -1 when the focus is
// on the name row or the buttons.
func (lp *layoutPresetFlow) focusedSlot() int {
	if lp.focus >= 1 && lp.focus <= lp.paneCount() {
		return lp.focus - 1
	}
	return -1
}

// paneCommandFlags reports, per slot, whether the pane has a non-empty startup
// command — drives the ✓ marker in the preview. A pane with no command just
// opens a plain shell, so assigning commands is never required to save.
func (lp *layoutPresetFlow) paneCommandFlags() []bool {
	flags := make([]bool, len(lp.commands))
	for i, c := range lp.commands {
		flags[i] = c != ""
	}
	return flags
}

// openLayoutPresetCreate opens the flow at the pick stage. Returns false (with
// a user message) when the fleet has no active sessions to capture from.
func (fleetPage *fleetPage) openLayoutPresetCreate(m *model) bool {
	candidates := fleetPage.collectLayoutPresetCandidates(m, fleetPage.dlg.fleet)
	if len(candidates) == 0 {
		m.message = "No active sessions to capture a layout from"
		return false
	}
	fleetPage.lpFlow = &layoutPresetFlow{
		stage:      lpStagePick,
		editIdx:    -1,
		candidates: candidates,
	}
	fleetPage.mode = viewLayoutPreset
	return true
}

// openLayoutPresetEdit opens the flow directly at the edit stage for the idx-th
// existing preset in the dialog's working copy.
func (fleetPage *fleetPage) openLayoutPresetEdit(idx int) {
	if idx < 0 || idx >= len(fleetPage.editFleet.layoutPresets) {
		return
	}
	preset := fleetPage.editFleet.layoutPresets[idx]
	lp := &layoutPresetFlow{
		stage:   lpStageEdit,
		editIdx: idx,
	}
	lp.initEditStage(preset.Layout, preset.PaneCount(), preset.Name, slices.Clone(preset.PaneCommands))
	fleetPage.lpFlow = lp
	fleetPage.mode = viewLayoutPreset
}

// collectLayoutPresetCandidates lists every live session group across the
// fleet's instances, labeled "<instance> / <group>". A group that has a saved
// outer-tmux layout (the same snapshot group restore uses) carries that
// geometry; one that was never opened as a split has none and falls back to
// the default stacked arrangement.
func (fleetPage *fleetPage) collectLayoutPresetCandidates(m *model, fleetName string) []lpCandidate {
	f, ok := m.st.Fleets[fleetName]
	if !ok {
		return nil
	}
	var out []lpCandidate
	for _, inst := range f.Instances {
		ref := InstanceRef{Fleet: fleetName, Instance: inst.Name}
		sessions := m.runtimeSessions(ref)
		if len(sessions) == 0 {
			continue
		}
		sanitized := SanitizeSessionName(inst.Name)
		for _, g := range groupSessions(sanitized, sessions) {
			paneCount := len(g.Sessions)
			layout := ""
			if sg, ok := fleetPage.savedGroups[computeGroupKey(inst.Name, g.GroupID)]; ok {
				layout = sg.Layout
				paneCount = savedGroupPaneCount(sg)
			}
			out = append(out, lpCandidate{
				instance:  inst.Name,
				display:   inst.GetDisplayName() + " / " + g.GroupID,
				groupName: g.GroupID,
				layout:    layout,
				paneCount: paneCount,
			})
		}
	}
	return out
}

// initEditStage populates the edit-stage state from a layout string + pane
// count. When the layout fails to parse — or disagrees with the pane count —
// the geometry falls back to the synthesized stack that applying a layout-less
// preset would actually produce, so the preview never lies about the result.
func (lp *layoutPresetFlow) initEditStage(layout string, paneCount int, name string, commands []string) {
	if paneCount < 1 {
		paneCount = 1
	}
	lp.layout = layout
	lp.rects = nil
	if layout != "" {
		if rects, err := parseTmuxLayout(layout); err == nil && len(rects)-1 == paneCount {
			lp.rects = rects
		}
	}
	if lp.rects == nil {
		lp.rects = synthesizedStackRects(paneCount)
		lp.layout = ""
	}
	lp.order = orderRectsByPosition(lp.rects)

	if len(commands) != paneCount {
		resized := make([]string, paneCount)
		copy(resized, commands)
		commands = resized
	}
	lp.commands = commands

	lp.nameInput = textinput.New()
	lp.nameInput.Placeholder = "preset-name"
	lp.nameInput.CharLimit = 64
	lp.nameInput.SetValue(name)
	lp.cmdInput = textinput.New()
	lp.cmdInput.Placeholder = "bash command (empty for plain shell)"
	lp.cmdInput.CharLimit = 512

	lp.stage = lpStageEdit
	lp.focus = 1 // first pane: assigning commands is the main task
	lp.editingName = false
	lp.editingCmd = false
	lp.errMsg = ""
}

// paneCountLabel formats a pane count for display ("1 pane", "3 panes").
func paneCountLabel(n int) string {
	if n == 1 {
		return "1 pane"
	}
	return fmt.Sprintf("%d panes", n)
}

// uniquePresetName seeds the name field from the captured group's name,
// suffixing -2, -3, … when that name is already taken by another preset.
func uniquePresetName(base string, presets []fleet.LayoutPreset, skipIdx int) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "layout"
	}
	taken := func(name string) bool {
		for i, p := range presets {
			if i != skipIdx && p.Name == name {
				return true
			}
		}
		return false
	}
	if !taken(base) {
		return base
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		if !taken(candidate) {
			return candidate
		}
	}
}

// closeLayoutPresetFlow abandons the flow and returns to the edit-fleet
// dialog, leaving the Layouts section expanded with the cursor where the user
// left it.
func (fleetPage *fleetPage) closeLayoutPresetFlow() {
	fleetPage.lpFlow = nil
	fleetPage.mode = viewEditFleet
}

// updateLayoutPreset handles all input while the preset flow is open.
func (fleetPage *fleetPage) updateLayoutPreset(m *model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	lp := fleetPage.lpFlow
	if lp == nil {
		// Defensive: a stray message after close.
		fleetPage.mode = viewEditFleet
		return nil
	}
	if lp.stage == lpStagePick {
		return fleetPage.updateLayoutPresetPick(lp, keyMsg)
	}
	return fleetPage.updateLayoutPresetEdit(m, lp, keyMsg)
}

func (fleetPage *fleetPage) updateLayoutPresetPick(lp *layoutPresetFlow, keyMsg tea.KeyMsg) tea.Cmd {
	switch keyMsg.String() {
	case "up", "k":
		lp.pickCursor = (lp.pickCursor - 1 + len(lp.candidates)) % len(lp.candidates)
	case "down", "j", "tab":
		lp.pickCursor = (lp.pickCursor + 1) % len(lp.candidates)
	case "enter", " ":
		c := lp.candidates[lp.pickCursor]
		name := uniquePresetName(c.groupName, fleetPage.editFleet.layoutPresets, -1)
		lp.initEditStage(c.layout, c.paneCount, name, nil)
	case "esc", "q", "Q", "ctrl+c":
		fleetPage.closeLayoutPresetFlow()
	}
	return nil
}

func (fleetPage *fleetPage) updateLayoutPresetEdit(m *model, lp *layoutPresetFlow, keyMsg tea.KeyMsg) tea.Cmd {
	// Name text sub-mode.
	if lp.editingName {
		switch keyMsg.String() {
		case "enter":
			lp.editingName = false
			lp.nameInput.Blur()
			return nil
		case "esc":
			// Discard the uncommitted edit, matching the home-dir field.
			lp.nameInput.SetValue(lp.nameBeforeEdit)
			lp.editingName = false
			lp.nameInput.Blur()
			return nil
		case "ctrl+c":
			fleetPage.closeLayoutPresetFlow()
			return nil
		}
		lp.errMsg = ""
		var cmd tea.Cmd
		lp.nameInput, cmd = lp.nameInput.Update(keyMsg)
		return cmd
	}

	// Pane-command text sub-mode.
	if lp.editingCmd {
		slot := lp.focusedSlot()
		switch keyMsg.String() {
		case "enter":
			if slot >= 0 {
				lp.commands[slot] = strings.TrimSpace(lp.cmdInput.Value())
			}
			lp.editingCmd = false
			lp.cmdInput.Blur()
			lp.advanceAfterCommand()
			return nil
		case "esc":
			lp.editingCmd = false
			lp.cmdInput.Blur()
			return nil
		case "ctrl+c":
			fleetPage.closeLayoutPresetFlow()
			return nil
		}
		var cmd tea.Cmd
		lp.cmdInput, cmd = lp.cmdInput.Update(keyMsg)
		return cmd
	}

	// Navigation.
	switch keyMsg.String() {
	case "esc", "q", "Q", "ctrl+c":
		fleetPage.closeLayoutPresetFlow()
		return nil

	// Arrow / hjkl keys move spatially over the preview's regions (name above,
	// the pane grid in the middle, the buttons below) so a multi-column layout
	// navigates the way it looks — "down" stays in a column instead of walking
	// to the next pane in position order. Tab keeps the flat cycle.
	case "up", "k":
		lp.navVertical(-1)
		return nil

	case "down", "j":
		lp.navVertical(1)
		return nil

	case "left", "h":
		lp.navHorizontal(-1)
		return nil

	case "right", "l":
		lp.navHorizontal(1)
		return nil

	case "tab":
		lp.moveFocus(1)
		return nil

	case "shift+tab":
		lp.moveFocus(-1)
		return nil

	case "enter", " ":
		return fleetPage.activateLayoutPresetFocus(m, lp)
	}

	// Start typing directly on the name row or a pane row, matching the
	// home-dir / add-mount convention.
	if isDialogTextKey(keyMsg) {
		if lp.focus == lpFocusName {
			lp.beginEditName()
			blinkCmd := lp.nameInput.Cursor.BlinkCmd()
			var inputCmd tea.Cmd
			lp.nameInput, inputCmd = lp.nameInput.Update(keyMsg)
			return tea.Batch(blinkCmd, inputCmd)
		}
		if slot := lp.focusedSlot(); slot >= 0 {
			lp.beginEditCommand(slot)
			blinkCmd := lp.cmdInput.Cursor.BlinkCmd()
			var inputCmd tea.Cmd
			lp.cmdInput, inputCmd = lp.cmdInput.Update(keyMsg)
			return tea.Batch(blinkCmd, inputCmd)
		}
	}
	return nil
}

// moveFocus steps through the focus stops (name → panes → cancel → save),
// wrapping. The save button is always reachable — assigning per-pane commands
// is optional, so there is no gating. Bound to Tab; the arrow/hjkl keys use the
// spatial navigation below instead.
func (lp *layoutPresetFlow) moveFocus(delta int) {
	maxFocus := lp.focusConfirm()
	lp.focus = (lp.focus + delta + maxFocus + 1) % (maxFocus + 1)
}

// The preview is laid out as three stacked regions — the name row on top, the
// pane grid in the middle, the cancel/save buttons on the bottom — so spatial
// navigation treats them in that vertical order and moves between panes by
// their on-screen geometry rather than their flat position-order index.

// slotCenter returns the screen-space center of the pane at slot (in the
// captured layout's cell coordinates).
func (lp *layoutPresetFlow) slotCenter(slot int) (int, int) {
	r := lp.rects[lp.order[slot]]
	return r.x + r.w/2, r.y + r.h/2
}

// windowWidth is the captured layout's full width (max right edge), used to
// decide which button a downward move from a pane lands on.
func (lp *layoutPresetFlow) windowWidth() int {
	w := 0
	for _, r := range lp.rects {
		w = max(w, r.x+r.w)
	}
	return w
}

// paneInDir returns the slot of the best pane in direction (dx, dy) from slot s
// — strictly in that direction, preferring panes aligned on the cross axis — or
// -1 when there is none. (dy>0 is down, dx>0 is right.)
func (lp *layoutPresetFlow) paneInDir(s, dx, dy int) int {
	cx, cy := lp.slotCenter(s)
	best, bestScore := -1, 0
	for slot := range lp.order {
		if slot == s {
			continue
		}
		nx, ny := lp.slotCenter(slot)
		if dy > 0 && ny <= cy || dy < 0 && ny >= cy {
			continue
		}
		if dx > 0 && nx <= cx || dx < 0 && nx >= cx {
			continue
		}
		// Distance along the move axis, plus a heavy penalty for drifting on
		// the cross axis so neighbors in the same row/column win.
		var score int
		if dy != 0 {
			score = absInt(ny-cy) + 2*absInt(nx-cx)
		} else {
			score = absInt(nx-cx) + 2*absInt(ny-cy)
		}
		if best == -1 || score < bestScore {
			best, bestScore = slot, score
		}
	}
	return best
}

// bottomPaneForButton returns the bottom-most pane nearest the given button's
// side (save → right, cancel → left), where an upward move from a button lands.
func (lp *layoutPresetFlow) bottomPaneForButton(button int) int {
	wantRight := button == lp.focusConfirm()
	best := 0
	bx, by := lp.slotCenter(0)
	for slot := 1; slot < lp.paneCount(); slot++ {
		cx, cy := lp.slotCenter(slot)
		better := cy > by || (cy == by && (wantRight && cx > bx || !wantRight && cx < bx))
		if better {
			best, bx, by = slot, cx, cy
		}
	}
	return best
}

// navVertical moves the focus up (dir<0) or down (dir>0) across the regions:
// name → pane grid → buttons, wrapping at the ends.
func (lp *layoutPresetFlow) navVertical(dir int) {
	switch {
	case lp.focus == lpFocusName:
		if dir > 0 {
			lp.focus = 1 // top-left pane (slot 0, position order)
		} else {
			lp.focus = lp.focusCancel() // wrap up to the buttons
		}
	case lp.focusedSlot() >= 0:
		if next := lp.paneInDir(lp.focusedSlot(), 0, dir); next >= 0 {
			lp.focus = 1 + next
		} else if dir > 0 {
			// No pane below: drop to the button under this pane's column.
			cx, _ := lp.slotCenter(lp.focusedSlot())
			if cx*2 >= lp.windowWidth() {
				lp.focus = lp.focusConfirm()
			} else {
				lp.focus = lp.focusCancel()
			}
		} else {
			lp.focus = lpFocusName
		}
	default: // on a button
		if dir > 0 {
			lp.focus = lpFocusName // wrap down to the name row
		} else {
			lp.focus = 1 + lp.bottomPaneForButton(lp.focus)
		}
	}
}

// navHorizontal moves left (dir<0) or right (dir>0): between adjacent panes in
// the grid, or between the cancel and save buttons. The name row is a single
// full-width field, so horizontal movement there is a no-op.
func (lp *layoutPresetFlow) navHorizontal(dir int) {
	switch {
	case lp.focusedSlot() >= 0:
		if next := lp.paneInDir(lp.focusedSlot(), dir, 0); next >= 0 {
			lp.focus = 1 + next
		}
	case lp.focus == lp.focusCancel() && dir > 0:
		lp.focus = lp.focusConfirm()
	case lp.focus == lp.focusConfirm() && dir < 0:
		lp.focus = lp.focusCancel()
	}
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// advanceAfterCommand moves the focus to the next pane after a command is set,
// so the happy path is enter → type command → enter, repeated; on the last
// pane it lands on the save button. Walking to save is just a convenience —
// the user can move to it (or cancel) with the arrow keys at any time.
func (lp *layoutPresetFlow) advanceAfterCommand() {
	if s := lp.focusedSlot(); s >= 0 && s < lp.paneCount()-1 {
		lp.focus = 1 + s + 1
		return
	}
	lp.focus = lp.focusConfirm()
}

func (lp *layoutPresetFlow) beginEditName() {
	lp.editingName = true
	lp.nameBeforeEdit = lp.nameInput.Value()
	lp.errMsg = ""
	lp.nameInput.Focus()
}

func (lp *layoutPresetFlow) beginEditCommand(slot int) {
	lp.editingCmd = true
	lp.errMsg = ""
	lp.cmdInput.SetValue(lp.commands[slot])
	lp.cmdInput.Focus()
}

// activateLayoutPresetFocus handles enter/space on the current focus stop.
func (fleetPage *fleetPage) activateLayoutPresetFocus(m *model, lp *layoutPresetFlow) tea.Cmd {
	switch {
	case lp.focus == lpFocusName:
		lp.beginEditName()
		return lp.nameInput.Cursor.BlinkCmd()
	case lp.focusedSlot() >= 0:
		lp.beginEditCommand(lp.focusedSlot())
		return lp.cmdInput.Cursor.BlinkCmd()
	case lp.focus == lp.focusCancel():
		fleetPage.closeLayoutPresetFlow()
		return nil
	case lp.focus == lp.focusConfirm():
		return fleetPage.commitLayoutPreset(m, lp)
	}
	return nil
}

// commitLayoutPreset validates and persists the preset (instant-save like the
// rest of the edit-fleet dialog), then returns to the Layouts section with the
// cursor on the saved preset's row.
func (fleetPage *fleetPage) commitLayoutPreset(m *model, lp *layoutPresetFlow) tea.Cmd {
	name := strings.TrimSpace(lp.nameInput.Value())
	if name == "" {
		lp.errMsg = "preset name is required"
		lp.focus = lpFocusName
		return nil
	}
	for i, p := range fleetPage.editFleet.layoutPresets {
		if i != lp.editIdx && p.Name == name {
			lp.errMsg = fmt.Sprintf("a preset named %q already exists", name)
			lp.focus = lpFocusName
			return nil
		}
	}

	preset := fleet.LayoutPreset{
		Name:         name,
		Layout:       lp.layout,
		PaneCommands: slices.Clone(lp.commands),
	}

	prev := fleetPage.editFleet.layoutPresets
	next := slices.Clone(prev)
	savedIdx := lp.editIdx
	if savedIdx >= 0 && savedIdx < len(next) {
		next[savedIdx] = preset
	} else {
		next = append(next, preset)
		savedIdx = len(next) - 1
	}
	fleetPage.editFleet.layoutPresets = next
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.editFleet.layoutPresets = prev
		m.message = fmt.Sprintf("Failed to save: %v", err)
		return nil
	}

	fleetPage.lpFlow = nil
	fleetPage.mode = viewEditFleet
	fleetPage.editFleet.layoutsExpanded = true
	fleetPage.dlg.row = editFleetRowLayoutPresetBase + savedIdx
	return nil
}

// ===========================================
// Rendering
// ===========================================

// renderLayoutPresetDialog builds the dialog body for the current stage.
func (fleetPage *fleetPage) renderLayoutPresetDialog() string {
	lp := fleetPage.lpFlow
	if lp == nil {
		return ""
	}
	if lp.stage == lpStagePick {
		return lp.renderPickStage()
	}
	return lp.renderEditStage()
}

func (lp *layoutPresetFlow) renderPickStage() string {
	var d strings.Builder
	d.WriteString(dialogTitle.Render("New layout preset"))
	d.WriteString("\n\n")
	d.WriteString(dialogLabel.Render("Capture the layout of:"))
	d.WriteString("\n\n")
	for i, c := range lp.candidates {
		if i == lp.pickCursor {
			d.WriteString(cursorStyle.Render("> ") + selectedStyle.Render(c.display))
		} else {
			d.WriteString("  " + dialogLabel.Render(c.display))
		}
		if c.paneCount > 1 {
			d.WriteString(dimStyle.Render("  (" + paneCountLabel(c.paneCount) + ")"))
		}
		d.WriteString("\n")
	}
	d.WriteString("\n")
	d.WriteString(dialogHint.Render("[j/k] Select  [enter] Capture  [q/esc] Cancel"))
	return d.String()
}

func (lp *layoutPresetFlow) renderEditStage() string {
	marker := func(focused bool) string {
		if focused {
			return cursorStyle.Render("> ")
		}
		return "  "
	}

	var d strings.Builder
	title := "New layout preset"
	if lp.editIdx >= 0 {
		title = "Edit layout preset"
	}
	d.WriteString(dialogTitle.Render(title))
	d.WriteString("\n\n")

	// Name row.
	var nameField string
	if lp.editingName {
		nameField = lp.nameInput.View()
	} else if v := lp.nameInput.Value(); v == "" {
		nameField = dimStyle.Render("(unnamed)")
	} else {
		nameField = v
	}
	d.WriteString(marker(lp.focus == lpFocusName) + dialogLabel.Render("Name: ") + nameField + "\n\n")

	// The layout preview. The ✓ marks panes that carry a startup command.
	width, height := previewDims(lp.rects)
	d.WriteString(renderLayoutPreview(lp.rects, lp.order, lp.focusedSlot(), lp.paneCommandFlags(), width, height))
	d.WriteString("\n")

	// Context line under the preview: the focused pane's command (or the
	// in-progress edit). A pane with no command just opens a plain shell.
	switch {
	case lp.editingCmd:
		d.WriteString("\n" + dialogLabel.Render(fmt.Sprintf("Pane %d command: ", lp.focusedSlot()+1)) + lp.cmdInput.View() + "\n")
	case lp.focusedSlot() >= 0:
		slot := lp.focusedSlot()
		if cmd := lp.commands[slot]; cmd == "" {
			d.WriteString("\n" + dimStyle.Render(fmt.Sprintf("pane %d: plain shell — enter to set a command", slot+1)) + "\n")
		} else {
			d.WriteString("\n" + dimStyle.Render(fmt.Sprintf("pane %d: ", slot+1)) + dialogLabel.Render(cmd) + "\n")
		}
	default:
		d.WriteString("\n" + dimStyle.Render("panes run their command when a session is created") + "\n")
	}

	if lp.errMsg != "" {
		d.WriteString(errorStyle.Render("✗ "+lp.errMsg) + "\n")
	}

	// Button row: cancel and save, both arrow-navigable. Saving is allowed at
	// any time — per-pane commands are optional (an empty one is a plain shell).
	cancel := "[ cancel ]"
	if lp.focus == lp.focusCancel() {
		cancel = selectedStyle.Render(cancel)
	} else {
		cancel = dimStyle.Render(cancel)
	}
	save := "[ save ]"
	if lp.focus == lp.focusConfirm() {
		save = selectedStyle.Render(save)
	} else {
		save = dimStyle.Render(save)
	}
	d.WriteString("\n" + cancel + "                       " + save + "\n")

	d.WriteString(dialogHint.Render(lp.editStageHint()))
	return d.String()
}

func (lp *layoutPresetFlow) editStageHint() string {
	if lp.editingName {
		return "[enter] Done  [esc] Done  [ctrl+c] Cancel"
	}
	if lp.editingCmd {
		return "[enter] Set  [esc] Back  [ctrl+c] Cancel"
	}
	if lp.focusedSlot() >= 0 {
		return "[enter] Set command  [j/k/h/l] Navigate  [q/esc] Cancel"
	}
	return "[enter] Select  [j/k/h/l] Navigate  [q/esc] Cancel"
}
