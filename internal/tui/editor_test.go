package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestBuildEditorCommandRoundTrip drives the real shell invocation with a
// non-interactive fake editor that CARRIES AN ARGUMENT (proving `sh -c` handles
// editors-with-args like "code --wait"): it must receive the temp-file path and
// the edit must round-trip back from disk.
func TestBuildEditorCommandRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(path, []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", "")
	// $EDITOR is itself `sh -c <script> _`, so the real path lands as $1.
	t.Setenv("EDITOR", `sh -c 'printf "edited\n" > "$1"' _`)

	if err := buildEditorCommand(path).Run(); err != nil {
		t.Fatalf("fake editor run: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimRight(string(got), "\n") != "edited" {
		t.Fatalf("file not edited through the editor command: %q", got)
	}
}

func TestResolveEditorPrecedence(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	if got := resolveEditor(); got != "vi" {
		t.Errorf("with neither set, editor = %q, want vi", got)
	}
	t.Setenv("EDITOR", "nano")
	if got := resolveEditor(); got != "nano" {
		t.Errorf("EDITOR = %q, want nano", got)
	}
	t.Setenv("VISUAL", "code --wait")
	if got := resolveEditor(); got != "code --wait" {
		t.Errorf("VISUAL should win over EDITOR, got %q", got)
	}
}

func TestShellQuoteSingle(t *testing.T) {
	if got := shQuote("/tmp/a b.md"); got != "'/tmp/a b.md'" {
		t.Errorf("spaces: got %q", got)
	}
	if got := shQuote("it's"); got != `'it'\''s'` {
		t.Errorf("embedded quote: got %q", got)
	}
}

func TestPromptFieldPreview(t *testing.T) {
	if got := promptFieldPreview("", "placeholder"); !strings.Contains(got, "placeholder") {
		t.Errorf("empty value should show the placeholder, got %q", got)
	}
	if got := promptFieldPreview("one line", "ph"); got != "one line" {
		t.Errorf("single line preview = %q, want %q", got, "one line")
	}
	multi := promptFieldPreview("first line\nsecond\nthird", "ph")
	if !strings.Contains(multi, "first line") || !strings.Contains(multi, "(3 lines)") {
		t.Errorf("multi-line preview = %q, want first line + (3 lines)", multi)
	}
}

func TestEditorResultSetsAgentSysPrompt(t *testing.T) {
	m, fp := newAutomationModel(t)
	fp.openAddAgentDialog(m, "alpha")
	fp.Update(m, editorFinishedMsg{target: editorTargetAgentSysPrompt, value: "line1\nline2"})
	if fp.agentDlg.systemPrompt != "line1\nline2" {
		t.Errorf("sys prompt = %q, want the edited value", fp.agentDlg.systemPrompt)
	}
}

func TestEditorResultSetsTriggerPrompt(t *testing.T) {
	m, fp := newAutomationModel(t)
	fp.openAddTriggerDialog(m, "alpha")
	fp.Update(m, editorFinishedMsg{target: editorTargetTriggerPrompt, value: "do the thing"})
	if fp.triggerDlg.prompt != "do the thing" {
		t.Errorf("trigger prompt = %q, want the edited value", fp.triggerDlg.prompt)
	}
}

func TestEditorResultErrorSurfacesMessageAndKeepsValue(t *testing.T) {
	m, fp := newAutomationModel(t)
	fp.openAddAgentDialog(m, "alpha")
	fp.agentDlg.systemPrompt = "keep me"
	fp.Update(m, editorFinishedMsg{target: editorTargetAgentSysPrompt, err: errors.New("boom")})
	if !strings.Contains(m.message, "boom") {
		t.Errorf("editor error should surface in the message, got %q", m.message)
	}
	if fp.agentDlg.systemPrompt != "keep me" {
		t.Errorf("an editor error must leave the field unchanged, got %q", fp.agentDlg.systemPrompt)
	}
}

func TestAgentEnterOnSysPromptOpensEditor(t *testing.T) {
	orig := editorCmd
	var gotTarget editorTarget
	var gotCurrent string
	called := false
	editorCmd = func(target editorTarget, _, current string) tea.Cmd {
		called, gotTarget, gotCurrent = true, target, current
		return nil
	}
	defer func() { editorCmd = orig }()

	m, fp := newAutomationModel(t)
	fp.openAddAgentDialog(m, "alpha")
	fp.agentDlg.systemPrompt = "existing prompt"
	fp.agentDlg.row = agentRowSystemPrompt
	fp.agentRowEnter(m)

	if !called || gotTarget != editorTargetAgentSysPrompt || gotCurrent != "existing prompt" {
		t.Fatalf("editor not launched for sys prompt: called=%v target=%v current=%q", called, gotTarget, gotCurrent)
	}
	if fp.agentDlg.fieldActive {
		t.Error("the sys prompt is editor-backed and must not inline-activate")
	}
}

func TestAgentEnterOnNameActivatesInline(t *testing.T) {
	orig := editorCmd
	called := false
	editorCmd = func(editorTarget, string, string) tea.Cmd { called = true; return nil }
	defer func() { editorCmd = orig }()

	m, fp := newAutomationModel(t)
	fp.openAddAgentDialog(m, "alpha")
	fp.agentDlg.row = agentRowName
	fp.agentRowEnter(m)

	if called {
		t.Error("the name field must not open the editor")
	}
	if !fp.agentDlg.fieldActive {
		t.Error("the name field should inline-activate")
	}
}

func TestAgentTypingOnSysPromptDoesNotInlineActivate(t *testing.T) {
	orig := editorCmd
	editorCmd = func(editorTarget, string, string) tea.Cmd { return nil }
	defer func() { editorCmd = orig }()

	m, fp := newAutomationModel(t)
	fp.openAddAgentDialog(m, "alpha")
	fp.agentDlg.row = agentRowSystemPrompt
	fp.updateAutomationAgent(m, key('x'))
	if fp.agentDlg.fieldActive {
		t.Error("typing on the editor-backed sys prompt must not inline-activate it")
	}
}

func TestTriggerEnterOnPromptOpensEditor(t *testing.T) {
	orig := editorCmd
	var gotTarget editorTarget
	var gotCurrent string
	called := false
	editorCmd = func(target editorTarget, _, current string) tea.Cmd {
		called, gotTarget, gotCurrent = true, target, current
		return nil
	}
	defer func() { editorCmd = orig }()

	m, fp := newAutomationModel(t)
	fp.openAddTriggerDialog(m, "alpha")
	fp.triggerDlg.prompt = "trigger prompt"
	fp.triggerDlg.row = trigRowPrompt
	fp.triggerRowEnter(m)

	if !called || gotTarget != editorTargetTriggerPrompt || gotCurrent != "trigger prompt" {
		t.Fatalf("editor not launched for trigger prompt: called=%v target=%v current=%q", called, gotTarget, gotCurrent)
	}
	if fp.triggerDlg.fieldActive {
		t.Error("the trigger prompt is editor-backed and must not inline-activate")
	}
}
