package tui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/inspector"
	homedircheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/homedir"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// editFleetRow identifies a focusable row in the edit-fleet dialog.
const (
	editFleetRowAgents = iota // collapsible section header (issue #184)
	editFleetRowClaude        // child of Agents; only navigable when expanded
	editFleetRowCodex         // child of Agents; only navigable when expanded
	editFleetRowAuggie        // child of Agents; only navigable when expanded
	editFleetRowGh
	editFleetRowHomeDir
	editFleetRowPreferFleetLaunch
	editFleetRowLayouts       // collapsible section header (issue #150)
	editFleetRowCustomMounts  // collapsible section header
	editFleetRowCaching       // collapsible section header
	editFleetRowBuildkit      // child of Caching; only navigable when expanded
	editFleetRowDebCache      // child of Caching; only navigable when expanded
	editFleetRowImageCache    // child of Caching; only navigable when expanded
	editFleetRowCoder         // collapsible section header (issue #221)
	editFleetRowCoderWsName   // child of Coder; workspace-name override text input
	editFleetRowCoderTemplate // child of Coder; template text input (commit kicks a param fetch)
	editFleetRowCoderPreset   // child of Coder; preset cycler (values from the last fetch)
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

// editFleetRowCoderParamBase is the start of the dynamic coder-parameter child
// rows (issue #221), a third dynamic band above the layout-preset one. Row
// base+i is the i-th template parameter (the list comes from the template
// fetch; there is no add row).
const editFleetRowCoderParamBase = 3000

// isCustomMountChildRow reports whether row is one of the dynamic custom-mount
// child rows (an existing mount or the "+ Add mount" row).
func isCustomMountChildRow(row int) bool {
	return row >= editFleetRowCustomMountBase && row < editFleetRowLayoutPresetBase
}

// isLayoutPresetChildRow reports whether row is one of the dynamic
// layout-preset child rows (an existing preset or the "+ Layout Preset" row).
func isLayoutPresetChildRow(row int) bool {
	return row >= editFleetRowLayoutPresetBase && row < editFleetRowCoderParamBase
}

// isCoderParamChildRow reports whether row is one of the dynamic
// coder-parameter child rows.
func isCoderParamChildRow(row int) bool { return row >= editFleetRowCoderParamBase }

// customMountAddRow returns the row id of the "+ Add mount" affordance, which
// always sits just past the last existing custom mount.
func (fleetPage *fleetPage) customMountAddRow() int {
	return editFleetRowCustomMountBase + len(fleetPage.editFleet.customMounts)
}

// layoutPresetAddRow returns the row id of the "+ Layout Preset" affordance,
// which always sits just past the last existing preset.
func (fleetPage *fleetPage) layoutPresetAddRow() int {
	return editFleetRowLayoutPresetBase + len(fleetPage.editFleet.layoutPresets)
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
		return fleetPage.editFleet.buildkitServer
	case cacheDeb:
		return fleetPage.editFleet.debCache
	case cacheImage:
		return fleetPage.editFleet.imageCache
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

// enabledAgentCount returns how many of the agent-tool mounts (Claude / Codex /
// Auggie) are currently toggled on, for the count shown in the Agents section
// header. GitHub CLI is intentionally excluded — it is a supporting tool, not a
// coding agent, and stays a top-level mount row (issue #184).
func (fleetPage *fleetPage) enabledAgentCount() int {
	n := 0
	for _, on := range []bool{
		fleetPage.editFleet.claudeMount,
		fleetPage.editFleet.codexMount,
		fleetPage.editFleet.auggieMount,
	} {
		if on {
			n++
		}
	}
	return n
}

// cacheRowFocused reports whether the dialog cursor is currently on the row for
// cache kind k (used so the [Delete cache] button only highlights its own row).
func (fleetPage *fleetPage) cacheRowFocused(k cacheKind) bool {
	rk, ok := cacheKindForRow(fleetPage.dlg.row)
	return ok && rk == k
}

// visibleEditFleetRows returns the edit-fleet dialog's navigable rows in display
// order. The agent-tool rows (Claude / Codex / Auggie) appear only while the
// Agents section is expanded; the custom-mount child rows appear only while that
// section is expanded (one per mount, then the add row); the Buildkit row only
// appears while the Caching section is expanded.
func (fleetPage *fleetPage) visibleEditFleetRows() []int {
	rows := []int{editFleetRowAgents}
	if fleetPage.editFleet.agentsExpanded {
		rows = append(rows, editFleetRowClaude, editFleetRowCodex, editFleetRowAuggie)
	}
	rows = append(rows,
		editFleetRowGh,
		editFleetRowHomeDir,
		editFleetRowPreferFleetLaunch,
		editFleetRowLayouts,
	)
	if fleetPage.editFleet.layoutsExpanded {
		for i := range fleetPage.editFleet.layoutPresets {
			rows = append(rows, editFleetRowLayoutPresetBase+i)
		}
		rows = append(rows, fleetPage.layoutPresetAddRow())
	}
	rows = append(rows, editFleetRowCustomMounts)
	if fleetPage.editFleet.customMountsExpanded {
		for i := range fleetPage.editFleet.customMounts {
			rows = append(rows, editFleetRowCustomMountBase+i)
		}
		rows = append(rows, fleetPage.customMountAddRow())
	}
	rows = append(rows, editFleetRowCaching)
	if fleetPage.editFleet.cachingExpanded {
		rows = append(rows, editFleetRowBuildkit, editFleetRowDebCache, editFleetRowImageCache)
	}
	rows = append(rows, editFleetRowCoder)
	if fleetPage.editFleet.coderExpanded {
		rows = append(rows, editFleetRowCoderWsName, editFleetRowCoderTemplate, editFleetRowCoderPreset)
		for i := range fleetPage.editFleet.coderParams {
			rows = append(rows, editFleetRowCoderParamBase+i)
		}
	}
	return rows
}

// moveEditFleetRow moves the dialog cursor by delta within the visible rows,
// wrapping, and resets the per-row sub-state (button focus / delete confirm).
func (fleetPage *fleetPage) moveEditFleetRow(delta int) {
	rows := fleetPage.visibleEditFleetRows()
	idx, found := 0, false
	for i, r := range rows {
		if r == fleetPage.dlg.row {
			idx, found = i, true
			break
		}
	}
	if found {
		fleetPage.dlg.row = rows[(idx+delta+len(rows))%len(rows)]
	} else {
		// The current row is no longer visible (e.g. its section collapsed under
		// the cursor) — recover by landing on the first visible row.
		fleetPage.dlg.row = rows[0]
	}
	fleetPage.editFleet.cacheButtonFocused = false
	fleetPage.editFleet.deleteCacheConfirm = false
	fleetPage.editFleet.mountRemoveConfirm = false
	fleetPage.editFleet.presetRemoveFocused = false
	fleetPage.editFleet.presetRemoveConfirm = false
	fleetPage.editFleet.presetMoveFocused = false
	fleetPage.editFleet.presetMoving = false
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
	fleetPage.dlg.fleet = f.Name
	fleetPage.editFleet.claudeMount = f.Settings.ClaudeCodeMount
	fleetPage.editFleet.codexMount = f.Settings.CodexMount
	fleetPage.editFleet.ghMount = f.Settings.GhMount
	fleetPage.editFleet.auggieMount = f.Settings.AuggieMount
	fleetPage.editFleet.buildkitServer = f.Settings.BuildkitServer
	fleetPage.editFleet.debCache = f.Settings.DebCacheServer
	fleetPage.editFleet.imageCache = f.Settings.ImageCacheServer
	fleetPage.editFleet.preferFleetLaunch = f.Settings.PreferFleetLaunchEnabled()
	fleetPage.editFleet.preferFleetLaunchSet = f.Settings.PreferFleetLaunchSet()
	fleetPage.dlg.row = editFleetRowAgents
	fleetPage.editFleet.agentsExpanded = false
	fleetPage.editFleet.detecting = false
	fleetPage.dlg.fieldActive = false
	fleetPage.editFleet.cachingExpanded = false
	fleetPage.editFleet.cacheButtonFocused = false
	fleetPage.editFleet.deleteCacheConfirm = false
	fleetPage.editFleet.deleting = false
	fleetPage.editFleet.customMountsExpanded = false
	fleetPage.editFleet.customMounts = slices.Clone(f.Settings.CustomMounts)
	fleetPage.editFleet.addingMount = false
	fleetPage.editFleet.customMountErr = ""
	fleetPage.editFleet.mountRemoveConfirm = false
	fleetPage.customMountInput.SetValue("")
	fleetPage.customMountInput.Blur()
	fleetPage.editFleet.layoutsExpanded = false
	fleetPage.editFleet.layoutPresets = slices.Clone(f.Settings.LayoutPresets)
	fleetPage.editFleet.presetRemoveFocused = false
	fleetPage.editFleet.presetRemoveConfirm = false
	fleetPage.editFleet.presetMoveFocused = false
	fleetPage.editFleet.presetMoving = false
	fleetPage.lpFlow = nil

	fleetPage.editFleet.coderExpanded = false
	fleetPage.editFleet.coderPreset = f.Settings.CoderPreset
	fleetPage.editFleet.coderParams = slices.Clone(f.Settings.CoderParameters)
	fleetPage.editFleet.coderPresets = nil
	fleetPage.editFleet.coderFetching = false
	fleetPage.coderWsNameInput.SetValue(f.Settings.CoderWorkspaceName)
	fleetPage.coderWsNameInput.Blur()
	fleetPage.coderTemplateInput.SetValue(f.Settings.CoderTemplate)
	fleetPage.coderTemplateInput.Blur()

	fleetPage.homedirInput.SetValue(f.Settings.HomeDir)
	fleetPage.homedirInput.Blur()

	var cmds []tea.Cmd
	// Refresh the template's parameter metadata + preset list so the Coder
	// section renders live values (mirrors the fetch the old global settings
	// page ran on startup).
	if f.Settings.CoderTemplate != "" {
		fleetPage.editFleet.coderFetching = true
		cmds = append(cmds, fetchCoderParamsCmd(f.Name, f.Settings.CoderTemplate))
	}
	if fleetPage.shouldKickHomedirDetect(f) {
		cmds = append(cmds, fleetPage.startHomedirDetect(f))
	}
	return tea.Batch(cmds...)
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

	// Text-editing sub-mode (home dir / coder workspace name / coder template /
	// coder parameter — whichever text row the cursor is on).
	if fleetPage.dlg.fieldActive {
		switch keyMsg.String() {
		case "enter":
			// Commit the typed value (instant-save) and leave editing.
			cmd := fleetPage.commitEditFleetField(m)
			fleetPage.dlg.fieldActive = false
			fleetPage.syncEditFleetFocus()
			return cmd
		case "esc":
			// Discard the uncommitted edit; restore the persisted value.
			fleetPage.restoreEditFleetField(m)
			fleetPage.dlg.fieldActive = false
			fleetPage.syncEditFleetFocus()
			return nil
		case "ctrl+c":
			fleetPage.closeEditFleet(m)
			return nil
		}
		if input := fleetPage.activeEditFleetInput(); input != nil {
			var cmd tea.Cmd
			*input, cmd = input.Update(msg)
			return cmd
		}
		return nil
	}

	// Add-custom-mount text-editing sub-mode.
	if fleetPage.editFleet.addingMount {
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
		fleetPage.editFleet.customMountErr = ""
		var cmd tea.Cmd
		fleetPage.customMountInput, cmd = fleetPage.customMountInput.Update(msg)
		return cmd
	}

	// Layout-preset reorder sub-mode: up/down drag the focused preset instead of
	// moving the dialog cursor, so it must be handled before the generic
	// navigation below.
	if fleetPage.editFleet.presetMoving {
		return fleetPage.updatePresetMove(m, keyMsg)
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
		if fleetPage.editFleet.deleteCacheConfirm {
			fleetPage.editFleet.deleteCacheConfirm = false
			return nil
		}
		if fleetPage.editFleet.mountRemoveConfirm {
			fleetPage.editFleet.mountRemoveConfirm = false
			return nil
		}
		if fleetPage.editFleet.presetRemoveConfirm {
			fleetPage.editFleet.presetRemoveConfirm = false
			return nil
		}
		fleetPage.closeEditFleet(m)
		return nil
	}

	// Dynamic custom-mount child rows (existing mounts + the add row) are
	// handled separately since their row ids are not compile-time constants.
	if isCustomMountChildRow(fleetPage.dlg.row) {
		return fleetPage.updateCustomMountRow(m, keyMsg)
	}
	// Likewise the dynamic layout-preset child rows (existing presets + the
	// "+ Layout Preset" row).
	if isLayoutPresetChildRow(fleetPage.dlg.row) {
		return fleetPage.updateLayoutPresetRow(m, keyMsg)
	}
	// And the dynamic coder-parameter child rows.
	if isCoderParamChildRow(fleetPage.dlg.row) {
		return fleetPage.updateCoderParamRow(keyMsg)
	}

	// Row-specific actions.
	switch fleetPage.dlg.row {
	case editFleetRowClaude, editFleetRowCodex, editFleetRowGh, editFleetRowAuggie, editFleetRowPreferFleetLaunch:
		// space/x and h/l/enter all toggle (instant-save), matching the
		// settings page.
		switch keyMsg.String() {
		case " ", "left", "right", "h", "l", "x", "enter":
			return fleetPage.toggleEditFleetRow(m)
		}
		return nil
	case editFleetRowAgents:
		switch keyMsg.String() {
		case " ", "enter":
			fleetPage.editFleet.agentsExpanded = !fleetPage.editFleet.agentsExpanded
		case "right", "l":
			fleetPage.editFleet.agentsExpanded = true
		case "left", "h":
			fleetPage.editFleet.agentsExpanded = false
		}
		return nil
	case editFleetRowCustomMounts:
		switch keyMsg.String() {
		case " ", "enter":
			fleetPage.editFleet.customMountsExpanded = !fleetPage.editFleet.customMountsExpanded
		case "right", "l":
			fleetPage.editFleet.customMountsExpanded = true
		case "left", "h":
			fleetPage.editFleet.customMountsExpanded = false
		}
		return nil
	case editFleetRowLayouts:
		switch keyMsg.String() {
		case " ", "enter":
			fleetPage.editFleet.layoutsExpanded = !fleetPage.editFleet.layoutsExpanded
		case "right", "l":
			fleetPage.editFleet.layoutsExpanded = true
		case "left", "h":
			fleetPage.editFleet.layoutsExpanded = false
		}
		return nil
	case editFleetRowCaching:
		switch keyMsg.String() {
		case " ", "enter":
			fleetPage.editFleet.cachingExpanded = !fleetPage.editFleet.cachingExpanded
		case "right", "l":
			fleetPage.editFleet.cachingExpanded = true
		case "left", "h":
			fleetPage.editFleet.cachingExpanded = false
		}
		return nil
	case editFleetRowCoder:
		switch keyMsg.String() {
		case " ", "enter":
			fleetPage.editFleet.coderExpanded = !fleetPage.editFleet.coderExpanded
		case "right", "l":
			fleetPage.editFleet.coderExpanded = true
		case "left", "h":
			fleetPage.editFleet.coderExpanded = false
		}
		return nil
	case editFleetRowCoderPreset:
		switch keyMsg.String() {
		case " ", "enter", "right", "l":
			return fleetPage.cycleFleetCoderPreset(m, 1)
		case "left", "h":
			return fleetPage.cycleFleetCoderPreset(m, -1)
		}
		return nil
	case editFleetRowCoderWsName, editFleetRowCoderTemplate:
		return fleetPage.beginEditFleetTextRow(keyMsg, msg)
	case editFleetRowBuildkit:
		return fleetPage.updateCacheRow(m, keyMsg, cacheBuildkit)
	case editFleetRowDebCache:
		return fleetPage.updateCacheRow(m, keyMsg, cacheDeb)
	case editFleetRowImageCache:
		return fleetPage.updateCacheRow(m, keyMsg, cacheImage)
	case editFleetRowHomeDir:
		return fleetPage.beginEditFleetTextRow(keyMsg, msg)
	}
	return nil
}

// beginEditFleetTextRow enters the text-editing sub-mode for the current text
// row (home dir / coder workspace name / coder template) on enter/space, or
// immediately on the first typed character (like the home-dir row always did).
func (fleetPage *fleetPage) beginEditFleetTextRow(keyMsg tea.KeyMsg, msg tea.Msg) tea.Cmd {
	input := fleetPage.activeEditFleetInputForRow(fleetPage.dlg.row)
	if input == nil {
		return nil
	}
	switch keyMsg.String() {
	case "enter", " ":
		fleetPage.dlg.fieldActive = true
		fleetPage.syncEditFleetFocus()
		return input.Cursor.BlinkCmd()
	}
	if isDialogTextKey(keyMsg) {
		fleetPage.dlg.fieldActive = true
		fleetPage.syncEditFleetFocus()
		blinkCmd := input.Cursor.BlinkCmd()
		var inputCmd tea.Cmd
		*input, inputCmd = input.Update(msg)
		return tea.Batch(blinkCmd, inputCmd)
	}
	return nil
}

// updateCoderParamRow handles a key press while the cursor is on one of the
// dynamic coder-parameter child rows: enter/space (or the first typed
// character) opens the shared parameter text input pre-loaded with the row's
// current value.
func (fleetPage *fleetPage) updateCoderParamRow(keyMsg tea.KeyMsg) tea.Cmd {
	idx := fleetPage.dlg.row - editFleetRowCoderParamBase
	if idx < 0 || idx >= len(fleetPage.editFleet.coderParams) {
		return nil
	}
	beginEdit := func() tea.Cmd {
		p := fleetPage.editFleet.coderParams[idx]
		fleetPage.coderParamInput.SetValue(p.Value)
		if p.DefaultValue != "" {
			fleetPage.coderParamInput.Placeholder = p.DefaultValue
		} else {
			fleetPage.coderParamInput.Placeholder = "value"
		}
		fleetPage.coderParamInput.CursorEnd()
		fleetPage.dlg.fieldActive = true
		fleetPage.syncEditFleetFocus()
		return fleetPage.coderParamInput.Cursor.BlinkCmd()
	}
	switch keyMsg.String() {
	case "enter", " ":
		return beginEdit()
	}
	if isDialogTextKey(keyMsg) {
		blinkCmd := beginEdit()
		var inputCmd tea.Cmd
		fleetPage.coderParamInput, inputCmd = fleetPage.coderParamInput.Update(keyMsg)
		return tea.Batch(blinkCmd, inputCmd)
	}
	return nil
}

// cycleFleetCoderPreset cycles the Coder preset selection through the fetched
// preset list (instant-save), a no-op until a fetch has populated it.
func (fleetPage *fleetPage) cycleFleetCoderPreset(m *model, direction int) tea.Cmd {
	presets := fleetPage.editFleet.coderPresets
	if len(presets) == 0 {
		return nil
	}
	idx := 0
	for i, preset := range presets {
		if preset == fleetPage.editFleet.coderPreset {
			idx = i
			break
		}
	}
	prev := fleetPage.editFleet.coderPreset
	fleetPage.editFleet.coderPreset = presets[(idx+direction+len(presets))%len(presets)]
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.editFleet.coderPreset = prev
		m.message = fmt.Sprintf("Failed to save: %v", err)
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
	if fleetPage.editFleet.deleting {
		return nil
	}
	switch keyMsg.String() {
	case " ", "x":
		// Toggling is a different action than confirming a delete, so always
		// disarm the confirm.
		fleetPage.editFleet.deleteCacheConfirm = false
		cmd := fleetPage.toggleEditFleetRow(m)
		if !fleetPage.cacheEnabled(k) {
			// No server → no button; drop button focus too.
			fleetPage.editFleet.cacheButtonFocused = false
		}
		return cmd
	case "right", "l":
		if fleetPage.cacheEnabled(k) {
			fleetPage.editFleet.cacheButtonFocused = true
		}
		return nil
	case "left", "h":
		fleetPage.editFleet.cacheButtonFocused = false
		fleetPage.editFleet.deleteCacheConfirm = false
		return nil
	case "enter":
		if fleetPage.editFleet.cacheButtonFocused && fleetPage.cacheEnabled(k) {
			if !fleetPage.editFleet.deleteCacheConfirm {
				fleetPage.editFleet.deleteCacheConfirm = true // first Enter: arm confirm
				return nil
			}
			fleetPage.editFleet.deleteCacheConfirm = false // second Enter: do it
			fleetPage.editFleet.deleting = true
			fleetPage.editFleet.deletingKind = k
			return deleteCacheCmd(k, fleetPage.dlg.fleet)
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
	if fleetPage.dlg.fleet == msg.fleet && fleetPage.editFleet.deletingKind == msg.kind {
		fleetPage.editFleet.deleting = false
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
	switch fleetPage.dlg.row {
	case editFleetRowClaude:
		fleetPage.editFleet.claudeMount = !fleetPage.editFleet.claudeMount
		turnedOn = fleetPage.editFleet.claudeMount
		revert = func() { fleetPage.editFleet.claudeMount = !fleetPage.editFleet.claudeMount }
	case editFleetRowCodex:
		fleetPage.editFleet.codexMount = !fleetPage.editFleet.codexMount
		turnedOn = fleetPage.editFleet.codexMount
		revert = func() { fleetPage.editFleet.codexMount = !fleetPage.editFleet.codexMount }
	case editFleetRowGh:
		fleetPage.editFleet.ghMount = !fleetPage.editFleet.ghMount
		turnedOn = fleetPage.editFleet.ghMount
		revert = func() { fleetPage.editFleet.ghMount = !fleetPage.editFleet.ghMount }
	case editFleetRowAuggie:
		fleetPage.editFleet.auggieMount = !fleetPage.editFleet.auggieMount
		turnedOn = fleetPage.editFleet.auggieMount
		revert = func() { fleetPage.editFleet.auggieMount = !fleetPage.editFleet.auggieMount }
	case editFleetRowBuildkit:
		fleetPage.editFleet.buildkitServer = !fleetPage.editFleet.buildkitServer
		revert = func() { fleetPage.editFleet.buildkitServer = !fleetPage.editFleet.buildkitServer }
	case editFleetRowDebCache:
		fleetPage.editFleet.debCache = !fleetPage.editFleet.debCache
		revert = func() { fleetPage.editFleet.debCache = !fleetPage.editFleet.debCache }
	case editFleetRowImageCache:
		fleetPage.editFleet.imageCache = !fleetPage.editFleet.imageCache
		revert = func() { fleetPage.editFleet.imageCache = !fleetPage.editFleet.imageCache }
	case editFleetRowPreferFleetLaunch:
		prevSet := fleetPage.editFleet.preferFleetLaunchSet
		fleetPage.editFleet.preferFleetLaunch = !fleetPage.editFleet.preferFleetLaunch
		// The user explicitly chose a value, so it must now persist.
		fleetPage.editFleet.preferFleetLaunchSet = true
		// Revert BOTH the value and the set-flag on save failure — otherwise a
		// failed toggle would leave editFleet.preferFleetLaunchSet=true and a later
		// unrelated save would collapse a "never asked" (nil) tri-state.
		revert = func() {
			fleetPage.editFleet.preferFleetLaunch = !fleetPage.editFleet.preferFleetLaunch
			fleetPage.editFleet.preferFleetLaunchSet = prevSet
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
		if f, ok := m.st.Fleets[fleetPage.dlg.fleet]; ok && fleetPage.shouldKickHomedirDetect(f) {
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
	idx := fleetPage.dlg.row - editFleetRowCustomMountBase
	if idx == len(fleetPage.editFleet.customMounts) {
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
		if !fleetPage.editFleet.mountRemoveConfirm {
			fleetPage.editFleet.mountRemoveConfirm = true // first press: arm confirm
			return nil
		}
		fleetPage.editFleet.mountRemoveConfirm = false // second press: do it
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
	idx := fleetPage.dlg.row - editFleetRowLayoutPresetBase
	if idx == len(fleetPage.editFleet.layoutPresets) {
		// The "+ Layout Preset" row.
		switch keyMsg.String() {
		case "enter", " ":
			fleetPage.openLayoutPresetCreate(m)
		}
		return nil
	}
	// Horizontal sub-cursor over the row's buttons: row → [remove] → [move].
	switch keyMsg.String() {
	case "right", "l":
		switch {
		case fleetPage.editFleet.presetMoveFocused:
			// Already at the rightmost button.
		case fleetPage.editFleet.presetRemoveFocused:
			fleetPage.editFleet.presetRemoveFocused = false
			fleetPage.editFleet.presetRemoveConfirm = false
			fleetPage.editFleet.presetMoveFocused = true
		default:
			fleetPage.editFleet.presetRemoveFocused = true
		}
		return nil
	case "left", "h":
		switch {
		case fleetPage.editFleet.presetMoveFocused:
			fleetPage.editFleet.presetMoveFocused = false
			fleetPage.editFleet.presetRemoveFocused = true
		case fleetPage.editFleet.presetRemoveFocused:
			fleetPage.editFleet.presetRemoveFocused = false
			fleetPage.editFleet.presetRemoveConfirm = false
		}
		return nil
	case "enter", " ":
		if fleetPage.editFleet.presetMoveFocused {
			// Start the reorder sub-mode; up/down now drag this preset.
			fleetPage.editFleet.presetMoving = true
			return nil
		}
		if fleetPage.editFleet.presetRemoveFocused {
			if !fleetPage.editFleet.presetRemoveConfirm {
				fleetPage.editFleet.presetRemoveConfirm = true // first Enter: arm confirm
				return nil
			}
			fleetPage.editFleet.presetRemoveConfirm = false // second Enter: remove
			return fleetPage.removeLayoutPreset(m, idx)
		}
		// Row focused (no button) → open the editor.
		fleetPage.openLayoutPresetEdit(idx)
		return nil
	}
	return nil
}

// updatePresetMove handles keys while the preset reorder sub-mode is active
// (entered with Enter on the [move] button). up/k and down/j drag the focused
// preset one slot, persisting on each swap (instant-save); any other navigation
// key — Enter, esc, q, h/l — commits the new order by leaving the sub-mode.
func (fleetPage *fleetPage) updatePresetMove(m *model, keyMsg tea.KeyMsg) tea.Cmd {
	switch keyMsg.String() {
	case "up", "k":
		return fleetPage.movePreset(m, -1)
	case "down", "j":
		return fleetPage.movePreset(m, 1)
	case "ctrl+c":
		fleetPage.closeEditFleet(m)
		return nil
	default:
		// Enter/space/esc/q/h/l (and anything else) leave the reorder sub-mode,
		// keeping the cursor on the [move] button of the preset's new position.
		fleetPage.editFleet.presetMoving = false
		return nil
	}
}

// movePreset swaps the focused preset with its neighbour delta slots away and
// persists the new order (instant-save), reverting on RPC failure. The dialog
// cursor follows the moved row so the user can keep dragging. Reordering the
// LayoutPresets slice is exactly what changes the layout-cycling order.
func (fleetPage *fleetPage) movePreset(m *model, delta int) tea.Cmd {
	idx := fleetPage.dlg.row - editFleetRowLayoutPresetBase
	target := idx + delta
	if idx < 0 || idx >= len(fleetPage.editFleet.layoutPresets) {
		return nil
	}
	if target < 0 || target >= len(fleetPage.editFleet.layoutPresets) {
		return nil // already at an edge — nothing to do
	}
	prev := fleetPage.editFleet.layoutPresets
	next := slices.Clone(prev)
	next[idx], next[target] = next[target], next[idx]
	fleetPage.editFleet.layoutPresets = next
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.editFleet.layoutPresets = prev
		m.message = fmt.Sprintf("Failed to save: %v", err)
		return nil
	}
	fleetPage.dlg.row = editFleetRowLayoutPresetBase + target
	return nil
}

// removeLayoutPreset drops the idx-th preset and persists (instant-save),
// reverting on RPC failure and keeping the cursor in range afterward.
func (fleetPage *fleetPage) removeLayoutPreset(m *model, idx int) tea.Cmd {
	if idx < 0 || idx >= len(fleetPage.editFleet.layoutPresets) {
		return nil
	}
	prev := fleetPage.editFleet.layoutPresets
	fleetPage.editFleet.layoutPresets = slices.Delete(slices.Clone(prev), idx, idx+1)
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.editFleet.layoutPresets = prev
		m.message = fmt.Sprintf("Failed to save: %v", err)
		return nil
	}
	if fleetPage.dlg.row > fleetPage.layoutPresetAddRow() {
		fleetPage.dlg.row = fleetPage.layoutPresetAddRow()
	}
	return nil
}

// beginAddMount enters the add-custom-mount text sub-mode with a blank input.
func (fleetPage *fleetPage) beginAddMount() {
	fleetPage.editFleet.addingMount = true
	fleetPage.editFleet.customMountErr = ""
	fleetPage.customMountInput.SetValue("")
	fleetPage.customMountInput.Focus()
}

// cancelAddMount leaves the add-custom-mount sub-mode, discarding the input.
func (fleetPage *fleetPage) cancelAddMount() {
	fleetPage.editFleet.addingMount = false
	fleetPage.editFleet.customMountErr = ""
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
		fleetPage.editFleet.customMountErr = err.Error()
		return nil
	}
	// Last-wins collisions with managed mounts are allowed, but an exact repeat
	// of an existing custom mount is a no-op — reject it with a clear message
	// rather than silently dropping it.
	if slices.Contains(fleetPage.editFleet.customMounts, norm) {
		fleetPage.editFleet.customMountErr = fmt.Sprintf("%s is already mounted", norm)
		return nil
	}

	prev := fleetPage.editFleet.customMounts
	fleetPage.editFleet.customMounts = append(slices.Clone(prev), norm)
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.editFleet.customMounts = prev
		m.message = fmt.Sprintf("Failed to save: %v", err)
		return nil
	}
	// Leave the cursor on the (now shifted-down) add row so the user can keep
	// adding mounts in a row.
	fleetPage.dlg.row = fleetPage.customMountAddRow()
	fleetPage.cancelAddMount()
	return nil
}

// removeCustomMount drops the idx-th custom mount and persists (instant-save),
// reverting on RPC failure and keeping the cursor in range afterward.
func (fleetPage *fleetPage) removeCustomMount(m *model, idx int) tea.Cmd {
	if idx < 0 || idx >= len(fleetPage.editFleet.customMounts) {
		return nil
	}
	prev := fleetPage.editFleet.customMounts
	fleetPage.editFleet.customMounts = slices.Delete(slices.Clone(prev), idx, idx+1)
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.editFleet.customMounts = prev
		m.message = fmt.Sprintf("Failed to save: %v", err)
		return nil
	}
	// Removing the last mount shrinks the visible row range; keep the cursor on
	// a row that still exists (at most the add row).
	if fleetPage.dlg.row > fleetPage.customMountAddRow() {
		fleetPage.dlg.row = fleetPage.customMountAddRow()
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
	if fleetPage.editFleet.detecting {
		return false
	}
	if strings.TrimSpace(fleetPage.homedirInput.Value()) != "" {
		return false
	}
	if !fleetPage.editFleet.claudeMount && !fleetPage.editFleet.codexMount && !fleetPage.editFleet.ghMount && !fleetPage.editFleet.auggieMount {
		return false
	}
	return strings.TrimSpace(f.Remote) != ""
}

// startHomedirDetect marks detection as in flight and returns the cmd
// that performs the actual clone+inspect work in the background.
func (fleetPage *fleetPage) startHomedirDetect(f *fleet.Fleet) tea.Cmd {
	fleetPage.editFleet.detecting = true
	return detectHomedirCmd(f.Name, f.Remote, "")
}

// handleHomedirDetected applies the result of an auto-detection and (instant
// save) persists the detected value. The guard checks ensure stale results —
// from a fleet the user has since closed, or arriving after the user has
// already typed a value — never overwrite live state.
func (fleetPage *fleetPage) handleHomedirDetected(m *model, msg homedirDetectedMsg) tea.Cmd {
	// Always clear the in-flight flag for *this* fleet so the spinner
	// stops, even when the result is not applied.
	if fleetPage.dlg.fleet == msg.fleetName {
		fleetPage.editFleet.detecting = false
	}
	if msg.err != nil || msg.homeDir == "" {
		return nil
	}
	if fleetPage.mode != viewEditFleet || fleetPage.dlg.fleet != msg.fleetName {
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

// activeEditFleetInputForRow returns the text input backing the given text
// row, nil for non-text rows. The coder-parameter rows share one input; its
// value is loaded on edit start and copied back on commit.
func (fleetPage *fleetPage) activeEditFleetInputForRow(row int) *textinput.Model {
	switch {
	case row == editFleetRowHomeDir:
		return &fleetPage.homedirInput
	case row == editFleetRowCoderWsName:
		return &fleetPage.coderWsNameInput
	case row == editFleetRowCoderTemplate:
		return &fleetPage.coderTemplateInput
	case isCoderParamChildRow(row):
		return &fleetPage.coderParamInput
	}
	return nil
}

// activeEditFleetInput returns the text input for the current cursor row.
func (fleetPage *fleetPage) activeEditFleetInput() *textinput.Model {
	return fleetPage.activeEditFleetInputForRow(fleetPage.dlg.row)
}

// commitEditFleetField commits the text-edit in progress on the current row
// (instant-save), dispatching to the row's commit handler.
func (fleetPage *fleetPage) commitEditFleetField(m *model) tea.Cmd {
	switch {
	case fleetPage.dlg.row == editFleetRowHomeDir:
		return fleetPage.commitHomedir(m)
	case fleetPage.dlg.row == editFleetRowCoderWsName:
		return fleetPage.commitCoderWsName(m)
	case fleetPage.dlg.row == editFleetRowCoderTemplate:
		return fleetPage.commitCoderTemplate(m)
	case isCoderParamChildRow(fleetPage.dlg.row):
		return fleetPage.commitCoderParam(m, fleetPage.dlg.row-editFleetRowCoderParamBase)
	}
	return nil
}

// restoreEditFleetField discards the uncommitted text-edit on the current row,
// restoring the input to the persisted value.
func (fleetPage *fleetPage) restoreEditFleetField(m *model) {
	f, ok := m.st.Fleets[fleetPage.dlg.fleet]
	if !ok {
		return
	}
	switch {
	case fleetPage.dlg.row == editFleetRowHomeDir:
		fleetPage.homedirInput.SetValue(f.Settings.HomeDir)
	case fleetPage.dlg.row == editFleetRowCoderWsName:
		fleetPage.coderWsNameInput.SetValue(f.Settings.CoderWorkspaceName)
	case fleetPage.dlg.row == editFleetRowCoderTemplate:
		fleetPage.coderTemplateInput.SetValue(f.Settings.CoderTemplate)
	}
	// Coder-parameter rows need no restore: the shared input is re-loaded from
	// the (unchanged) working copy the next time an edit starts.
}

// commitCoderWsName persists the workspace-name override (instant-save),
// restoring the input to the persisted value if the save fails (e.g. the
// server rejects an illegal coder name fragment).
func (fleetPage *fleetPage) commitCoderWsName(m *model) tea.Cmd {
	if err := fleetPage.persistFleetSettings(m); err != nil {
		if f, ok := m.st.Fleets[fleetPage.dlg.fleet]; ok {
			fleetPage.coderWsNameInput.SetValue(f.Settings.CoderWorkspaceName)
		}
		m.message = fmt.Sprintf("Failed to save: %v", err)
	}
	return nil
}

// commitCoderTemplate persists the template (instant-save) and, when it
// changed to a new non-empty value, kicks the parameter/preset fetch — exactly
// like the old global settings page did on a template commit.
func (fleetPage *fleetPage) commitCoderTemplate(m *model) tea.Cmd {
	prev := ""
	if f, ok := m.st.Fleets[fleetPage.dlg.fleet]; ok {
		prev = f.Settings.CoderTemplate
	}
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.coderTemplateInput.SetValue(prev)
		m.message = fmt.Sprintf("Failed to save: %v", err)
		return nil
	}
	template := strings.TrimSpace(fleetPage.coderTemplateInput.Value())
	if template != "" && template != prev {
		fleetPage.editFleet.coderFetching = true
		m.message = "Fetching template parameters..."
		return fetchCoderParamsCmd(fleetPage.dlg.fleet, template)
	}
	return nil
}

// commitCoderParam copies the shared parameter input into the idx-th binding
// and persists (instant-save), reverting the value on RPC failure.
func (fleetPage *fleetPage) commitCoderParam(m *model, idx int) tea.Cmd {
	if idx < 0 || idx >= len(fleetPage.editFleet.coderParams) {
		return nil
	}
	prev := fleetPage.editFleet.coderParams[idx].Value
	fleetPage.editFleet.coderParams[idx].Value = strings.TrimSpace(fleetPage.coderParamInput.Value())
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.editFleet.coderParams[idx].Value = prev
		m.message = fmt.Sprintf("Failed to save: %v", err)
	}
	return nil
}

// handleCoderParamsFetched applies a GetCoderTemplateParams result to the open
// edit-fleet dialog: fetched parameters are merged with the working copy
// (user-set values kept by name, metadata refreshed), the preset list is
// replaced, and an empty preset selection defaults to the first available.
// Stale results — the dialog closed or now shows a different fleet — are
// discarded (mirroring handleHomedirDetected).
func (fleetPage *fleetPage) handleCoderParamsFetched(m *model, msg coderParamsFetchedMsg) tea.Cmd {
	// Always clear the in-flight flag for *this* fleet so the spinner stops,
	// even when the result is not applied.
	if fleetPage.dlg.fleet == msg.fleetName {
		fleetPage.editFleet.coderFetching = false
	}
	if fleetPage.mode != viewEditFleet || fleetPage.dlg.fleet != msg.fleetName {
		return nil
	}
	if msg.err != nil {
		m.message = fmt.Sprintf("Failed to fetch parameters: %v", msg.err)
		return nil
	}

	// Merge parameters: keep existing user-set values, add new ones with the
	// template's metadata.
	existing := make(map[string]string)
	for _, param := range fleetPage.editFleet.coderParams {
		if param.Value != "" {
			existing[param.Name] = param.Value
		}
	}
	var newParams []fleet.CoderParameter
	for _, fetched := range msg.params {
		newParams = append(newParams, fleet.CoderParameter{
			Name:         fetched.Name,
			Value:        existing[fetched.Name],
			DefaultValue: fetched.DefaultValue,
			DisplayName:  fetched.DisplayName,
			Description:  fetched.Description,
			Type:         fetched.Type,
		})
	}

	prevParams := fleetPage.editFleet.coderParams
	prevPreset := fleetPage.editFleet.coderPreset
	fleetPage.editFleet.coderParams = newParams
	fleetPage.editFleet.coderPresets = slices.Clone(msg.presets)
	if fleetPage.editFleet.coderPreset == "" && len(msg.presets) > 0 {
		fleetPage.editFleet.coderPreset = msg.presets[0]
	}
	if err := fleetPage.persistFleetSettings(m); err != nil {
		fleetPage.editFleet.coderParams = prevParams
		fleetPage.editFleet.coderPreset = prevPreset
		m.message = fmt.Sprintf("Failed to save: %v", err)
		return nil
	}
	// The param list may have shrunk under the cursor; land back on a row
	// that still exists.
	if idx := fleetPage.dlg.row - editFleetRowCoderParamBase; isCoderParamChildRow(fleetPage.dlg.row) && idx >= len(newParams) {
		fleetPage.dlg.row = editFleetRowCoderPreset
	}
	m.message = fmt.Sprintf("Loaded %d parameters, %d presets", len(newParams), len(msg.presets))
	return nil
}

// syncEditFleetFocus moves the cursor blink to the text input backing the
// currently edited row. Every other input must blur to avoid a stray cursor.
func (fleetPage *fleetPage) syncEditFleetFocus() {
	var active *textinput.Model
	if fleetPage.dlg.fieldActive {
		active = fleetPage.activeEditFleetInput()
	}
	for _, input := range []*textinput.Model{
		&fleetPage.homedirInput,
		&fleetPage.coderWsNameInput,
		&fleetPage.coderTemplateInput,
		&fleetPage.coderParamInput,
	} {
		if input == active {
			input.Focus()
		} else {
			input.Blur()
		}
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
// user toggled it this session (editFleet.preferFleetLaunchSet) — so editing an
// unrelated setting never collapses a "never asked" (nil) into explicit false.
func (fleetPage *fleetPage) persistFleetSettings(m *model) error {
	f, ok := m.st.Fleets[fleetPage.dlg.fleet]
	if !ok {
		return fmt.Errorf("fleet %s not found", fleetPage.dlg.fleet)
	}
	prev := f.Settings
	f.Settings.ClaudeCodeMount = fleetPage.editFleet.claudeMount
	f.Settings.CodexMount = fleetPage.editFleet.codexMount
	f.Settings.GhMount = fleetPage.editFleet.ghMount
	f.Settings.AuggieMount = fleetPage.editFleet.auggieMount
	f.Settings.BuildkitServer = fleetPage.editFleet.buildkitServer
	f.Settings.CustomMounts = fleetPage.editFleet.customMounts
	f.Settings.LayoutPresets = fleetPage.editFleet.layoutPresets
	f.Settings.DebCacheServer = fleetPage.editFleet.debCache
	f.Settings.ImageCacheServer = fleetPage.editFleet.imageCache
	if fleetPage.editFleet.preferFleetLaunchSet {
		preferFleetLaunch := fleetPage.editFleet.preferFleetLaunch
		f.Settings.PreferFleetLaunch = &preferFleetLaunch
	}
	f.Settings.HomeDir = strings.TrimSpace(fleetPage.homedirInput.Value())
	f.Settings.CoderWorkspaceName = strings.TrimSpace(fleetPage.coderWsNameInput.Value())
	f.Settings.CoderTemplate = strings.TrimSpace(fleetPage.coderTemplateInput.Value())
	f.Settings.CoderPreset = fleetPage.editFleet.coderPreset
	f.Settings.CoderParameters = fleetPage.editFleet.coderParams

	if err := setFleetSettingsRemote(fleetPage.dlg.fleet, f.Settings); err != nil {
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
	if f, ok := m.st.Fleets[fleetPage.dlg.fleet]; ok {
		fleetPage.homedirInput.SetValue(f.Settings.HomeDir)
	}
}

// closeEditFleet closes the dialog. Instant-save means there is nothing to
// commit on close — every change was persisted as it was made.
func (fleetPage *fleetPage) closeEditFleet(_ *model) {
	fleetPage.mode = viewNormal
	fleetPage.editFleet.deleteCacheConfirm = false
	fleetPage.editFleet.cacheButtonFocused = false
	fleetPage.editFleet.presetMoveFocused = false
	fleetPage.editFleet.presetMoving = false
	// Clear the in-flight flag too: a wipe RPC may still be running, but its
	// deleteCacheDoneMsg is matched on fleet name and only updates the message,
	// so the dialog must not reopen showing a stale "Clearing…" spinner.
	fleetPage.editFleet.deleting = false
	fleetPage.blurDialogFields()
}

// ===========================================
// Port Forward Dialog
// ===========================================

// editFleetState holds the edit-fleet settings dialog: the shared-mount and
// caching toggles plus the collapsible Agents / Caching / Custom-mounts /
// Layouts sections. Every change is instant-saved; the layout-preset capture
// flow itself lives in fleetPage.lpFlow while mode == viewLayoutPreset.
type editFleetState struct {
	// Agents section. The coding-agent mounts (Claude / Codex / Auggie) live
	// under a collapsible "Agents" header (issue #184); GitHub CLI stays a
	// top-level mount row since it is a supporting tool, not a coding agent.
	agentsExpanded bool // ▼ Agents expanded, revealing the agent-tool rows

	claudeMount       bool
	codexMount        bool
	ghMount           bool
	auggieMount       bool
	buildkitServer    bool
	debCache          bool
	imageCache        bool
	preferFleetLaunch bool
	// preferFleetLaunchSet tracks whether PreferFleetLaunch should be
	// persisted as an explicit value. It starts true only if the fleet already
	// had a value, and flips true when the user toggles that row — so the
	// instant-save path never collapses a "never asked" (nil) PreferFleetLaunch
	// into an explicit false just because the user edited an unrelated setting.
	preferFleetLaunchSet bool

	// Caching section. The buildkit, deb, and image cache rows share one
	// interaction model (toggle + [Delete cache] button + inline confirm +
	// in-flight spinner); the per-row sub-state below applies to whichever
	// cache row currently has the dialog cursor.
	cachingExpanded    bool      // ▼ Caching expanded, revealing the cache rows
	cacheButtonFocused bool      // horizontal sub-cursor: on the [Delete cache] button vs the toggle
	deleteCacheConfirm bool      // inline confirm armed (first Enter on the button)
	deleting           bool      // a cache-wipe RPC is in flight
	deletingKind       cacheKind // which cache the in-flight wipe targets (valid only while deleting)

	// Custom mounts section.
	customMountsExpanded bool     // ▼ Custom mounts expanded, revealing per-mount rows + the add row
	customMounts         []string // working copy of the fleet's custom mounts (instant-save)
	addingMount          bool     // true while the "+ Add mount" text input is active
	customMountErr       string   // inline validation error shown under the add-mount input
	mountRemoveConfirm   bool     // inline "[remove?]" confirm armed on the focused custom-mount row (mirrors the Caching [Delete cache] flow)

	// Layouts section.
	layoutsExpanded     bool                 // ▼ Layouts expanded, revealing per-preset rows + the add row
	layoutPresets       []fleet.LayoutPreset // working copy of the fleet's layout presets (instant-save)
	presetRemoveFocused bool                 // horizontal sub-cursor: on the [remove] button vs the preset row (mirrors cacheButtonFocused)
	presetRemoveConfirm bool                 // inline "[remove?]" confirm armed on the focused preset row
	presetMoveFocused   bool                 // horizontal sub-cursor: on the [move] button (right of [remove])
	presetMoving        bool                 // reorder sub-mode: j/k drag the focused preset up/down (instant-save)

	// Coder section (issue #221). Template / workspace-name values live in
	// fleetPage.coderTemplateInput / coderWsNameInput (like homedirInput);
	// the preset selection and parameter working copy live here. The preset
	// list and parameter metadata come from the GetCoderTemplateParams fetch
	// kicked on dialog open (template already set) or on a template commit.
	coderExpanded bool                   // ▼ Coder expanded, revealing the coder rows
	coderPreset   string                 // current preset selection (instant-save)
	coderParams   []fleet.CoderParameter // working copy of the fleet's parameter bindings (instant-save)
	coderPresets  []string               // available preset names (in-memory, from the fetch)
	coderFetching bool                   // true while a template-params fetch is in flight

	detecting bool // true while a homedir auto-detect cmd is in flight
}

// renderEditFleet builds the edit-fleet dialog body from the currently visible
// rows. Instant-save means there is no explicit save row — toggles persist as
// they're made and esc/q just closes.
func (fleetPage *fleetPage) renderEditFleet(m *model) string {
	marker := func(row int) string {
		if fleetPage.dlg.row == row {
			return cursorStyle.Render("> ")
		}
		return "  "
	}
	checkbox := func(on bool) string {
		if on {
			return "[x]"
		}
		return "[ ]"
	}

	var d strings.Builder
	d.WriteString(dialogTitle.Render("Edit fleet"))
	d.WriteString("\n\n")
	d.WriteString("  " + dialogLabel.Render("Fleet:    ") + " " + fleetExpandedStyle.Render(fleetPage.dlg.fleet) + "\n")

	for _, row := range fleetPage.visibleEditFleetRows() {
		switch row {
		case editFleetRowAgents:
			arrow := "▶ "
			if fleetPage.editFleet.agentsExpanded {
				arrow = "▼ "
			}
			d.WriteString(marker(row) + dialogLabel.Render(fmt.Sprintf("%sAgents (%d)", arrow, fleetPage.enabledAgentCount())))
		case editFleetRowClaude:
			d.WriteString(marker(row) + "  " + checkbox(fleetPage.editFleet.claudeMount) + " " + dialogLabel.Render("Claude Code mount"))
		case editFleetRowCodex:
			d.WriteString(marker(row) + "  " + checkbox(fleetPage.editFleet.codexMount) + " " + dialogLabel.Render("Codex mount"))
		case editFleetRowAuggie:
			d.WriteString(marker(row) + "  " + checkbox(fleetPage.editFleet.auggieMount) + " " + dialogLabel.Render("Auggie mount"))
		case editFleetRowGh:
			d.WriteString(marker(row) + checkbox(fleetPage.editFleet.ghMount) + " " + dialogLabel.Render("GitHub CLI mount"))
		case editFleetRowHomeDir:
			// Text input when focused, dim static text otherwise; append a
			// spinner + status while an auto-detect runs.
			var field string
			if fleetPage.dlg.fieldActive && fleetPage.dlg.row == editFleetRowHomeDir {
				field = fleetPage.homedirInput.View()
			} else if v := fleetPage.homedirInput.Value(); v == "" {
				field = dimStyle.Render("(unset — defaults to /home/vscode)")
			} else {
				field = v
			}
			if fleetPage.editFleet.detecting {
				field += " " + m.spinner.View() + dimStyle.Render(" detecting home dir...")
			}
			d.WriteString(marker(row) + dialogLabel.Render("Home dir: ") + " " + field)
		case editFleetRowPreferFleetLaunch:
			d.WriteString(marker(row) + checkbox(fleetPage.editFleet.preferFleetLaunch) + " " + dialogLabel.Render("Prefer Fleet Launch"))
		case editFleetRowLayouts:
			arrow := "▶ "
			if fleetPage.editFleet.layoutsExpanded {
				arrow = "▼ "
			}
			d.WriteString(marker(row) + dialogLabel.Render(fmt.Sprintf("%sLayouts (%d)", arrow, len(fleetPage.editFleet.layoutPresets))))
		case editFleetRowCustomMounts:
			arrow := "▶ "
			if fleetPage.editFleet.customMountsExpanded {
				arrow = "▼ "
			}
			d.WriteString(marker(row) + dialogLabel.Render(fmt.Sprintf("%sCustom mounts (%d)", arrow, len(fleetPage.editFleet.customMounts))))
		case editFleetRowCaching:
			arrow := "▶ "
			if fleetPage.editFleet.cachingExpanded {
				arrow = "▼ "
			}
			d.WriteString(marker(row) + dialogLabel.Render(fmt.Sprintf("%sCaching (%d)", arrow, fleetPage.enabledCacheCount())))
		case editFleetRowBuildkit:
			d.WriteString(marker(row) + fleetPage.renderCacheRow(m, cacheBuildkit, "Buildkit server"))
		case editFleetRowDebCache:
			d.WriteString(marker(row) + fleetPage.renderCacheRow(m, cacheDeb, "Deb package cache"))
		case editFleetRowImageCache:
			d.WriteString(marker(row) + fleetPage.renderCacheRow(m, cacheImage, "Docker image cache"))
		case editFleetRowCoder:
			arrow := "▶ "
			if fleetPage.editFleet.coderExpanded {
				arrow = "▼ "
			}
			summary := strings.TrimSpace(fleetPage.coderTemplateInput.Value())
			if summary == "" {
				summary = "no template"
			}
			d.WriteString(marker(row) + dialogLabel.Render(fmt.Sprintf("%sCoder (%s)", arrow, summary)))
		case editFleetRowCoderWsName:
			var field string
			if fleetPage.dlg.fieldActive && fleetPage.dlg.row == editFleetRowCoderWsName {
				field = fleetPage.coderWsNameInput.View()
			} else if v := fleetPage.coderWsNameInput.Value(); v == "" {
				field = dimStyle.Render(fmt.Sprintf("(default: %s)", fleetPage.dlg.fleet))
			} else {
				field = v
			}
			d.WriteString(marker(row) + "  " + dialogLabel.Render("Workspace:") + " " + field)
		case editFleetRowCoderTemplate:
			var field string
			if fleetPage.dlg.fieldActive && fleetPage.dlg.row == editFleetRowCoderTemplate {
				field = fleetPage.coderTemplateInput.View()
			} else if v := fleetPage.coderTemplateInput.Value(); v == "" {
				field = dimStyle.Render("(not set)")
			} else {
				field = v
			}
			if fleetPage.editFleet.coderFetching {
				field += "  " + m.spinner.View() + dimStyle.Render(" fetching...")
			}
			d.WriteString(marker(row) + "  " + dialogLabel.Render("Template: ") + " " + field)
		case editFleetRowCoderPreset:
			preset := fleetPage.editFleet.coderPreset
			if preset == "" {
				preset = dimStyle.Render("(none)")
			} else {
				preset = fmt.Sprintf("[ %s ]", preset)
			}
			d.WriteString(marker(row) + "  " + dialogLabel.Render("Preset:   ") + " " + preset)
		default:
			// Dynamic child rows, indented one level under their section
			// header: layout presets, custom mounts, or coder parameters.
			switch {
			case isCoderParamChildRow(row):
				d.WriteString(fleetPage.renderCoderParamRow(row, marker))
			case isLayoutPresetChildRow(row):
				d.WriteString(fleetPage.renderLayoutPresetRow(row, marker))
			default:
				d.WriteString(fleetPage.renderCustomMountRow(row, marker))
			}
		}
		d.WriteString("\n")
	}

	if footer := fleetPage.customMountFooter(); footer != "" {
		d.WriteString("\n  " + footer + "\n")
	}
	if footer := fleetPage.coderFooter(); footer != "" {
		d.WriteString("\n  " + footer + "\n")
	}
	d.WriteString("\n  " + dimStyle.Render("Mounts apply on supported backends only") + "\n\n")
	d.WriteString(dialogHint.Render(fleetPage.editFleetHint()))
	return d.String()
}

// renderCustomMountRow renders one dynamic custom-mount child row: an existing
// mount (with a [remove] affordance) or the "+ Add mount" row (which becomes an
// inline text input while the add sub-mode is active).
func (fleetPage *fleetPage) renderCustomMountRow(row int, marker func(int) string) string {
	idx := row - editFleetRowCustomMountBase
	if idx == len(fleetPage.editFleet.customMounts) {
		// The "+ Add mount" row.
		if fleetPage.editFleet.addingMount {
			return marker(row) + "  " + dialogLabel.Render("New mount: ") + fleetPage.customMountInput.View()
		}
		return marker(row) + "  " + dialogLabel.Render("+ Add mount")
	}
	return marker(row) + "  " + fleetPage.editFleet.customMounts[idx] + "   " + fleetPage.renderRemoveMountButton(row)
}

// renderLayoutPresetRow renders one dynamic layout-preset child row: an
// existing preset (name + pane count, with a [remove] affordance) or the
// "+ Layout Preset" row that starts the capture flow.
func (fleetPage *fleetPage) renderLayoutPresetRow(row int, marker func(int) string) string {
	idx := row - editFleetRowLayoutPresetBase
	if idx == len(fleetPage.editFleet.layoutPresets) {
		return marker(row) + "  " + dialogLabel.Render("+ Layout Preset")
	}
	p := fleetPage.editFleet.layoutPresets[idx]
	label := fmt.Sprintf("%s (%s)", p.Name, paneCountLabel(p.PaneCount()))
	return marker(row) + "  " + label + "   " + fleetPage.renderRemovePresetButton(row) + " " + fleetPage.renderMovePresetButton(row)
}

// renderRemovePresetButton renders the [remove] affordance next to an existing
// layout preset. Like the Caching section's [Delete cache] button it is only
// highlighted when the horizontal sub-cursor is on it (dialogPresetRemoveFocused
// on this row) — selecting the row alone leaves it dim, so it never looks armed
// before the user arrows onto it.
func (fleetPage *fleetPage) renderRemovePresetButton(row int) string {
	focused := fleetPage.dlg.row == row && fleetPage.editFleet.presetRemoveFocused
	if focused && fleetPage.editFleet.presetRemoveConfirm {
		return selectedStyle.Render("[remove?]")
	}
	if focused {
		return selectedStyle.Render("[remove]")
	}
	return dimStyle.Render("[remove]")
}

// renderMovePresetButton renders the [move] affordance to the right of [remove].
// It is dim unless the horizontal sub-cursor is on it; once the user presses
// Enter to start dragging, it shows a highlighted "[moving]" so it is obvious
// that j/k now reorder the preset rather than navigate the dialog.
func (fleetPage *fleetPage) renderMovePresetButton(row int) string {
	focused := fleetPage.dlg.row == row && fleetPage.editFleet.presetMoveFocused
	if focused && fleetPage.editFleet.presetMoving {
		return selectedStyle.Render("[moving]")
	}
	if focused {
		return selectedStyle.Render("[move]")
	}
	return dimStyle.Render("[move]")
}

// renderRemoveMountButton renders the [remove] affordance next to an existing
// custom mount. It mirrors the Caching section's [Delete cache] button: dim when
// its row is not focused, highlighted when focused, and shown as a highlighted
// "[remove?]" once the inline confirm is armed.
func (fleetPage *fleetPage) renderRemoveMountButton(row int) string {
	focused := fleetPage.dlg.row == row
	if focused && fleetPage.editFleet.mountRemoveConfirm {
		return selectedStyle.Render("[remove?]")
	}
	if focused {
		return selectedStyle.Render("[remove]")
	}
	return dimStyle.Render("[remove]")
}

// renderCoderParamRow renders one dynamic coder-parameter child row: the
// parameter's display name (or name) and its value — the shared text input
// while the row is being edited, the template default (dim) when unset.
func (fleetPage *fleetPage) renderCoderParamRow(row int, marker func(int) string) string {
	idx := row - editFleetRowCoderParamBase
	if idx < 0 || idx >= len(fleetPage.editFleet.coderParams) {
		return marker(row)
	}
	param := fleetPage.editFleet.coderParams[idx]
	label := param.Name
	if param.DisplayName != "" {
		label = param.DisplayName
	}
	var value string
	if fleetPage.dlg.fieldActive && fleetPage.dlg.row == row {
		value = fleetPage.coderParamInput.View()
	} else if param.Value == "" {
		if param.DefaultValue != "" {
			value = dimStyle.Render(param.DefaultValue + " (default)")
		} else {
			value = dimStyle.Render("(not set)")
		}
	} else {
		value = param.Value
	}
	return marker(row) + "  " + dialogLabel.Render(label+": ") + value
}

// coderFooter returns a context line shown beneath the dialog rows while the
// cursor is on a Coder row: the ${...} interpolation variables on a parameter
// row, and the naming scheme on the workspace-name row.
func (fleetPage *fleetPage) coderFooter() string {
	if isCoderParamChildRow(fleetPage.dlg.row) {
		return dimStyle.Render("Variables: ${GIT_URL} = fleet repo URL, ${GIT_BRANCH} = git branch\n  (blank = default), ${INSTANCE_NAME} = workspace name")
	}
	if fleetPage.dlg.row == editFleetRowCoderWsName {
		return dimStyle.Render("workspaces are named <workspace>-<instance>")
	}
	return ""
}

// customMountFooter returns a context line shown beneath the dialog rows while
// the cursor is on a custom-mount row: the resolved host path for an existing
// mount or the in-progress add, plus a hint or inline validation error.
func (fleetPage *fleetPage) customMountFooter() string {
	if !isCustomMountChildRow(fleetPage.dlg.row) {
		return ""
	}
	idx := fleetPage.dlg.row - editFleetRowCustomMountBase
	if idx < len(fleetPage.editFleet.customMounts) {
		return dimStyle.Render("host: " + customMountHostPreview(fleetPage.dlg.fleet, fleetPage.editFleet.customMounts[idx]))
	}
	// The add row.
	if !fleetPage.editFleet.addingMount {
		return ""
	}
	if fleetPage.editFleet.customMountErr != "" {
		return errorStyle.Render("✗ " + fleetPage.editFleet.customMountErr)
	}
	if v := strings.TrimSpace(fleetPage.customMountInput.Value()); v != "" {
		return dimStyle.Render("host: " + customMountHostPreview(fleetPage.dlg.fleet, v))
	}
	return dimStyle.Render("enter an absolute container path, e.g. /opt/data")
}

// renderCacheRow renders one cache row (checkbox + label, indented under the
// Caching header) plus its [Delete cache] button when the cache is enabled. The
// three cache rows (buildkit/deb/image) share this rendering.
func (fleetPage *fleetPage) renderCacheRow(m *model, k cacheKind, label string) string {
	box := "[ ]"
	if fleetPage.cacheEnabled(k) {
		box = "[x]"
	}
	line := "  " + box + " " + dialogLabel.Render(label)
	if fleetPage.cacheEnabled(k) {
		line += "   " + fleetPage.renderDeleteCacheButton(m, k)
	}
	return line
}

// renderDeleteCacheButton renders the [Delete cache] button shown next to an
// enabled cache. It reflects the in-flight / inline-confirm state for cache kind
// k and is highlighted when the horizontal sub-cursor is on that row's button.
func (fleetPage *fleetPage) renderDeleteCacheButton(m *model, k cacheKind) string {
	var label string
	switch {
	case fleetPage.editFleet.deleting && fleetPage.editFleet.deletingKind == k:
		label = m.spinner.View() + " Clearing…"
	case fleetPage.cacheRowFocused(k) && fleetPage.editFleet.deleteCacheConfirm:
		// Kept short so the row fits the 46-col dialog; the footer hint spells
		// out enter=confirm / esc=cancel.
		label = "Delete cache?"
	default:
		label = "Delete cache"
	}
	text := "[ " + label + " ]"
	if fleetPage.cacheRowFocused(k) && fleetPage.editFleet.cacheButtonFocused {
		return selectedStyle.Render(text)
	}
	return dimStyle.Render(text)
}

func (fleetPage *fleetPage) editFleetHint() string {
	if fleetPage.dlg.fieldActive {
		return "[enter] Save  [esc] Discard edit"
	}
	if fleetPage.editFleet.addingMount {
		return "[enter] Add mount  [esc] Cancel"
	}
	if isCustomMountChildRow(fleetPage.dlg.row) {
		if fleetPage.dlg.row == fleetPage.customMountAddRow() {
			return "[enter] Add mount  [j/k] Select  [q/esc] Save & Close"
		}
		if fleetPage.editFleet.mountRemoveConfirm {
			return "[enter] Confirm remove  [esc] Cancel"
		}
		return "[enter/d] Remove  [j/k] Select  [q/esc] Save & Close"
	}
	if isCoderParamChildRow(fleetPage.dlg.row) {
		return "[enter] Edit  [j/k] Select  [q/esc] Save & Close"
	}
	if isLayoutPresetChildRow(fleetPage.dlg.row) {
		if fleetPage.dlg.row == fleetPage.layoutPresetAddRow() {
			return "[enter] New preset  [j/k] Select  [q/esc] Save & Close"
		}
		if fleetPage.editFleet.presetMoving {
			return "[j/k] Move  [enter/esc] Done"
		}
		if fleetPage.editFleet.presetMoveFocused {
			return "[enter] Move  [h/←] Back  [esc] Close"
		}
		if fleetPage.editFleet.presetRemoveFocused {
			if fleetPage.editFleet.presetRemoveConfirm {
				return "[enter] Confirm remove  [esc] Cancel"
			}
			return "[enter] Remove  [l/→] Move  [h/←] Back  [esc] Close"
		}
		return "[enter] Edit  [l/→] Buttons  [j/k] Select  [q/esc] Save & Close"
	}
	switch fleetPage.dlg.row {
	case editFleetRowAgents:
		if fleetPage.editFleet.agentsExpanded {
			return "[h/←] Collapse  [j/k] Select  [q/esc] Save & Close"
		}
		return "[l/→/space] Expand  [j/k] Select  [q/esc] Save & Close"
	case editFleetRowCustomMounts:
		if fleetPage.editFleet.customMountsExpanded {
			return "[h/←] Collapse  [j/k] Select  [q/esc] Save & Close"
		}
		return "[l/→/space] Expand  [j/k] Select  [q/esc] Save & Close"
	case editFleetRowLayouts:
		if fleetPage.editFleet.layoutsExpanded {
			return "[h/←] Collapse  [j/k] Select  [q/esc] Save & Close"
		}
		return "[l/→/space] Expand  [j/k] Select  [q/esc] Save & Close"
	case editFleetRowCaching:
		if fleetPage.editFleet.cachingExpanded {
			return "[h/←] Collapse  [j/k] Select  [q/esc] Save & Close"
		}
		return "[l/→/space] Expand  [j/k] Select  [q/esc] Save & Close"
	case editFleetRowCoder:
		if fleetPage.editFleet.coderExpanded {
			return "[h/←] Collapse  [j/k] Select  [q/esc] Save & Close"
		}
		return "[l/→/space] Expand  [j/k] Select  [q/esc] Save & Close"
	case editFleetRowCoderPreset:
		return "[h/l] Cycle  [j/k] Select  [q/esc] Save & Close"
	case editFleetRowCoderWsName, editFleetRowCoderTemplate:
		return "[enter] Edit  [j/k] Select  [q/esc] Save & Close"
	case editFleetRowBuildkit, editFleetRowDebCache, editFleetRowImageCache:
		if fleetPage.editFleet.cacheButtonFocused {
			if fleetPage.editFleet.deleteCacheConfirm {
				return "[enter] Confirm delete  [esc] Cancel"
			}
			return "[enter] Delete cache  [h/←] Back  [esc] Close"
		}
		if k, ok := cacheKindForRow(fleetPage.dlg.row); ok && fleetPage.cacheEnabled(k) {
			return "[space] Toggle  [l/→] Delete-cache button  [j/k] Select"
		}
		return "[space] Toggle  [j/k] Select  [q/esc] Save & Close"
	case editFleetRowHomeDir:
		return "[enter] Edit  [j/k] Select  [q/esc] Save & Close"
	}
	// Flat checkbox rows: Enter/space/h/l all toggle (instant-save).
	return "[j/k] Select  [space/enter/h/l] Toggle  [q/esc] Save & Close"
}

func (fleetPage *fleetPage) renderEditFleetDialog(m *model) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(dialogBox.Render(fleetPage.renderEditFleet(m)))
	b.WriteString("\n")

	return b.String()
}
