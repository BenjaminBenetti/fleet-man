package agentdetect

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeHookScript_RunnableEndToEnd executes the embedded hook
// script in a real subprocess, then parses the resulting state file
// with ParseClaudeStateFile. This is the canary that locks the
// wire-format contract between the script and the parser: rename
// either side without updating the other and this test fails.
//
// We exercise every event fleet-man manages plus an unknown event
// (which the script must classify as "unknown" and the parser must
// then refuse to interpret).
func TestClaudeHookScript_RunnableEndToEnd(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available on this host: %v", err)
	}

	cases := []struct {
		event     string
		wantOK    bool
		wantState State
	}{
		{"UserPromptSubmit", true, StateWorking},
		{"PreToolUse", true, StateWorking},
		{"PostToolUse", true, StateWorking},
		{"Notification", true, StateWaiting},
		{"Stop", true, StateWaiting},
		{"SomeUnknownEvent", false, StateNotRunning},
	}

	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			tmp := t.TempDir()
			scriptPath := filepath.Join(tmp, "fleet-man-state-hook.sh")
			stateDir := filepath.Join(tmp, "state")

			if err := os.WriteFile(scriptPath, ClaudeHookScript, 0o755); err != nil {
				t.Fatalf("write script: %v", err)
			}

			cmd := exec.Command(sh, scriptPath, tc.event)
			cmd.Env = append(os.Environ(), "FLEET_MAN_STATE_DIR="+stateDir)
			// Simulate Claude Code piping a JSON event payload — the
			// script must drain stdin without parsing.
			cmd.Stdin = strings.NewReader(`{"hook_event_name":"` + tc.event + `","cwd":"/x"}`)

			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("script exited non-zero: %v\noutput: %s", err, out)
			}

			stateFile := filepath.Join(stateDir, "claude-state")
			content, err := os.ReadFile(stateFile)
			if err != nil {
				t.Fatalf("read state file: %v", err)
			}

			reading, ok := ParseClaudeStateFile(string(content))
			if ok != tc.wantOK {
				t.Errorf("parser ok=%v, want %v (raw=%q)", ok, tc.wantOK, content)
			}
			if ok && reading.State != tc.wantState {
				t.Errorf("state=%d, want %d (raw=%q)", reading.State, tc.wantState, content)
			}
		})
	}
}

// TestClaudeHookScript_AtomicReplace verifies that running the
// script a second time replaces the file contents rather than
// appending. The script writes via mv; an accidental append would
// produce two lines and the parser would still read the first
// (stale) one.
func TestClaudeHookScript_AtomicReplace(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}

	tmp := t.TempDir()
	scriptPath := filepath.Join(tmp, "fleet-man-state-hook.sh")
	stateDir := filepath.Join(tmp, "state")

	if err := os.WriteFile(scriptPath, ClaudeHookScript, 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	run := func(event string) {
		cmd := exec.Command(sh, scriptPath, event)
		cmd.Env = append(os.Environ(), "FLEET_MAN_STATE_DIR="+stateDir)
		cmd.Stdin = strings.NewReader("{}")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("script(%s) failed: %v\noutput: %s", event, err, out)
		}
	}

	run("PreToolUse")
	run("Stop")

	content, err := os.ReadFile(filepath.Join(stateDir, "claude-state"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	// File should contain exactly one line: the most recent write.
	lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 line after second run, got %d: %q", len(lines), content)
	}
	reading, ok := ParseClaudeStateFile(string(content))
	if !ok || reading.State != StateWaiting {
		t.Errorf("second write did not replace first: state=%d ok=%v raw=%q",
			reading.State, ok, content)
	}
}
