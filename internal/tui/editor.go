package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// editor.go backs the "edit this field in $EDITOR" experience for the automation
// dialogs' long free-text fields — an agent's system prompt and a trigger's
// prompt. Inline single-line editing is miserable for many-line text, so
// selecting one of these fields opens the user's $EDITOR on a temp file and
// reads the result back when they exit.

// editorTarget identifies which dialog field an $EDITOR session feeds back into.
type editorTarget int

const (
	editorTargetAgentSysPrompt editorTarget = iota
	editorTargetTriggerPrompt
)

// editorFinishedMsg carries an $EDITOR session's result back to the dialog that
// launched it. On err the field is left unchanged.
type editorFinishedMsg struct {
	target editorTarget
	value  string
	err    error
}

// promptPreviewWidth caps the one-line preview an editor-backed field shows in
// the dialog. It is the total budget for the whole preview (first line +
// "(N lines)" suffix), sized to fit the narrowest caller: the agent dialog's
// "Sys prompt:" label leaves ~32 cols inside the 50-wide dialog box, so the
// preview never wraps to a second line.
const promptPreviewWidth = 32

// resolveEditor returns the user's preferred editor command — $VISUAL, then
// $EDITOR — falling back to vi (the universal default, always present).
func resolveEditor() string {
	if e := strings.TrimSpace(os.Getenv("VISUAL")); e != "" {
		return e
	}
	if e := strings.TrimSpace(os.Getenv("EDITOR")); e != "" {
		return e
	}
	return "vi"
}

// editorCmd launches $EDITOR on `current` (seeded into a temp file) and reads the
// result back into an editorFinishedMsg for `target`. It is a package var so
// tests can stub the side effects (temp file + process exec). The editor runs via
// `sh -c` so an $EDITOR carrying arguments (e.g. "code --wait") works — the same
// way git invokes its editor.
var editorCmd = func(target editorTarget, nameHint, current string) tea.Cmd {
	f, err := os.CreateTemp("", "fleet-"+nameHint+"-*.md")
	if err != nil {
		return func() tea.Msg { return editorFinishedMsg{target: target, err: err} }
	}
	path := f.Name()
	_, writeErr := f.WriteString(current)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(path)
		if writeErr == nil {
			writeErr = closeErr
		}
		return func() tea.Msg { return editorFinishedMsg{target: target, err: writeErr} }
	}

	return execProcess(buildEditorCommand(path), func(runErr error) tea.Msg {
		defer os.Remove(path)
		if runErr != nil {
			return editorFinishedMsg{target: target, err: runErr}
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return editorFinishedMsg{target: target, err: readErr}
		}
		// Drop the editor's trailing newline(s) so the value matches what inline
		// typing would have produced.
		return editorFinishedMsg{target: target, value: strings.TrimRight(string(data), "\n")}
	})
}

// buildEditorCommand returns the *exec.Cmd that opens the user's editor on path.
// $EDITOR runs through `sh -c` so an editor carrying arguments (e.g. "code
// --wait") is honored, exactly as git invokes GIT_EDITOR.
func buildEditorCommand(path string) *exec.Cmd {
	return exec.Command("sh", "-c", resolveEditor()+" "+shellQuoteSingle(path))
}

// shellQuoteSingle single-quotes s for safe inclusion in a `sh -c` string.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// applyEditorResult writes an $EDITOR session's result back into the dialog field
// it targeted. The dialog scratch survives the suspend — the TUI is paused while
// the editor runs, so the mode can't have changed underneath us.
func (fleetPage *fleetPage) applyEditorResult(m *model, msg editorFinishedMsg) tea.Cmd {
	if msg.err != nil {
		m.message = fmt.Sprintf("Editor error: %v", msg.err)
		return nil
	}
	switch msg.target {
	case editorTargetAgentSysPrompt:
		fleetPage.agentDlg.systemPrompt = msg.value
		fleetPage.autosaveAgent(m)
	case editorTargetTriggerPrompt:
		fleetPage.triggerDlg.prompt = msg.value
		fleetPage.autosaveTrigger(m)
	}
	return nil
}

// promptFieldPreview renders a one-line preview of an editor-backed field's
// (possibly multi-line) value for the dialog: the first line, truncated, with a
// dim line count when there is more than one line. Empty shows the placeholder.
// The first line is truncated to leave room for the suffix so the whole preview
// stays within promptPreviewWidth and never wraps to a second line.
func promptFieldPreview(value, placeholder string) string {
	if value == "" {
		return dimStyle.Render(placeholder)
	}
	lines := strings.Split(value, "\n")
	suffix := ""
	budget := promptPreviewWidth
	if len(lines) > 1 {
		suffix = fmt.Sprintf("  (%d lines)", len(lines))
		if budget -= len(suffix); budget < 1 {
			budget = 1
		}
	}
	preview := ansi.Truncate(lines[0], budget, "…")
	if suffix != "" {
		preview += dimStyle.Render(suffix)
	}
	return preview
}
