package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

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
	fleetPage.dlg.fieldActive = false
	fleetPage.editFleet.addingMount = false
	fleetPage.textInput.Blur()
	fleetPage.branchInput.Blur()
	fleetPage.homedirInput.Blur()
	fleetPage.customMountInput.Blur()
	fleetPage.coderWsNameInput.Blur()
	fleetPage.coderTemplateInput.Blur()
	fleetPage.coderParamInput.Blur()
}

func (fleetPage *fleetPage) activateTextInput() tea.Cmd {
	fleetPage.dlg.fieldActive = true
	fleetPage.textInput.Focus()
	return fleetPage.textInput.Cursor.BlinkCmd()
}

func (fleetPage *fleetPage) deactivateTextInput() {
	fleetPage.dlg.fieldActive = false
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

func (fleetPage *fleetPage) cancelTextDialog(m *model) tea.Cmd {
	fleetPage.mode = viewNormal
	fleetPage.blurDialogFields()
	m.message = "Cancelled"
	return nil
}

// ===========================================
// Add Fleet Dialog
// ===========================================

// dialogContext is the scratch every dialog reuses: which fleet/instance/
// session the action targets, the dialog's row cursor, and whether a text
// field is being edited. Only one dialog is open at a time (fleetPage.mode),
// so opening one stomps the previous one's context — that is by design.
type dialogContext struct {
	fleet       string
	inst        string
	session     string
	groupID     string
	row         int
	fieldActive bool
}

func (fleetPage *fleetPage) textDialogHint(action string) string {
	if fleetPage.dlg.fieldActive {
		return fmt.Sprintf("[enter] %s  [esc] Done editing  [ctrl+c] Cancel", action)
	}
	return "[enter] Edit  [q/esc] Cancel"
}
