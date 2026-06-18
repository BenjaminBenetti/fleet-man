package agentdetect

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAuggieHookScript_RunnableEndToEnd executes the embedded auggie
// hook script in a real subprocess, identifying the event via the
// AUGMENT_HOOK_EVENT environment variable the way auggie does, then
// parses the resulting state file. This locks the wire-format contract
// between the script and the shared parser.
func TestAuggieHookScript_RunnableEndToEnd(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available on this host: %v", err)
	}

	cases := []struct {
		event     string
		wantOK    bool
		wantState State
	}{
		{"SessionStart", true, StateWaiting},
		{"PromptSubmit", true, StateWorking},
		{"PreToolUse", true, StateWorking},
		{"PostToolUse", true, StateWorking},
		{"Notification", true, StateWaiting},
		{"Stop", true, StateWaiting},
		{"SomeUnknownEvent", false, StateNotRunning},
	}

	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			tmp := t.TempDir()
			scriptPath := filepath.Join(tmp, "auggie-state-hook.sh")
			stateDir := filepath.Join(tmp, "state")

			if err := os.WriteFile(scriptPath, AuggieHookScript, 0o755); err != nil {
				t.Fatalf("write script: %v", err)
			}

			// No positional arg: the script must read AUGMENT_HOOK_EVENT.
			cmd := exec.Command(sh, scriptPath)
			cmd.Env = append(os.Environ(),
				"FLEET_MAN_STATE_DIR="+stateDir,
				"AUGMENT_HOOK_EVENT="+tc.event,
			)
			// Simulate auggie piping a JSON event payload — the script
			// must drain stdin without parsing.
			cmd.Stdin = strings.NewReader(`{"hook_event_name":"` + tc.event + `","cwd":"/x"}`)

			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("script exited non-zero: %v\noutput: %s", err, out)
			}

			content, err := os.ReadFile(filepath.Join(stateDir, "auggie-state"))
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

// TestAuggieHookScript_ArgFallback verifies the script falls back to
// the first positional argument when AUGMENT_HOOK_EVENT is empty/unset.
// auggie also wires the event name into each hook entry's "args", so
// this path matters if a future spawn mode does not set the env var.
func TestAuggieHookScript_ArgFallback(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}

	tmp := t.TempDir()
	scriptPath := filepath.Join(tmp, "auggie-state-hook.sh")
	stateDir := filepath.Join(tmp, "state")
	if err := os.WriteFile(scriptPath, AuggieHookScript, 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// Event supplied as argv; AUGMENT_HOOK_EVENT explicitly empty so the
	// ${VAR:-${1:-unknown}} default chain reaches the positional arg.
	cmd := exec.Command(sh, scriptPath, "PreToolUse")
	cmd.Env = append(os.Environ(), "FLEET_MAN_STATE_DIR="+stateDir, "AUGMENT_HOOK_EVENT=")
	cmd.Stdin = strings.NewReader("{}")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\noutput: %s", err, out)
	}

	content, err := os.ReadFile(filepath.Join(stateDir, "auggie-state"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	reading, ok := ParseClaudeStateFile(string(content))
	if !ok || reading.State != StateWorking {
		t.Errorf("arg fallback failed: state=%d ok=%v raw=%q", reading.State, ok, content)
	}
}

// TestAuggieHookScript_AtomicReplace verifies a second run replaces the
// file rather than appending (the script writes via mv).
func TestAuggieHookScript_AtomicReplace(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}

	tmp := t.TempDir()
	scriptPath := filepath.Join(tmp, "auggie-state-hook.sh")
	stateDir := filepath.Join(tmp, "state")
	if err := os.WriteFile(scriptPath, AuggieHookScript, 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	run := func(event string) {
		cmd := exec.Command(sh, scriptPath)
		cmd.Env = append(os.Environ(), "FLEET_MAN_STATE_DIR="+stateDir, "AUGMENT_HOOK_EVENT="+event)
		cmd.Stdin = strings.NewReader("{}")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("script(%s) failed: %v\noutput: %s", event, err, out)
		}
	}

	run("PreToolUse")
	run("Stop")

	content, err := os.ReadFile(filepath.Join(stateDir, "auggie-state"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 line after second run, got %d: %q", len(lines), content)
	}
	reading, ok := ParseClaudeStateFile(string(content))
	if !ok || reading.State != StateWaiting {
		t.Errorf("second write did not replace first: state=%d ok=%v raw=%q", reading.State, ok, content)
	}
}
