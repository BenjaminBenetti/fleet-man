package agentdetect

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// ===========================================
// Local executor for end-to-end testing
// ===========================================

// localExecutor is a test ContainerExecutor that runs commands on
// the host machine with an overridden HOME. It mirrors the
// production BackendExecutor's stdin/stdout/stderr wiring so the
// provisioner exercises the real code paths it would in a live
// container; only the transport (process exec vs container exec)
// differs.
type localExecutor struct {
	home string
}

// Run satisfies ContainerExecutor.
func (l *localExecutor) Run(args []string, stdin []byte) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("localExecutor: empty args")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "HOME="+l.home)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// ===========================================
// End-to-end pipeline test
// ===========================================

// TestEndToEnd_ClaudeStateDetection exercises the entire Phase
// 1-through-4 pipeline against the host filesystem:
//
//	provisioner ──▶ drops script  ──┐
//	             ──▶ writes settings─┘
//	                                 ▼
//	(test simulating Claude Code fires hook for each event)
//	                                 │
//	                                 ▼
//	                         /tmp-equivalent/claude-state
//	                                 │
//	                                 ▼
//	            (test reads state file → constructs capture)
//	                                 │
//	                                 ▼
//	                      ClaudeHookDetector.Detect → State
//
// Each Claude Code lifecycle event we install a hook for is fired
// in turn, and the detector's reported state is asserted against
// the expected mapping. The test "container" is the host with
// $HOME redirected to a temp dir; FLEET_MAN_STATE_DIR likewise
// points at a temp dir so we never touch /tmp/fleet-man on the
// real host.
func TestEndToEnd_ClaudeStateDetection(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not available on this host: %v", err)
	}

	home := t.TempDir()
	stateDir := t.TempDir()

	// Phase 4: provisioner
	if err := NewClaudeProvisioner(&localExecutor{home: home}).Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Phase 4 contract: script lives at $HOME/<suffix> and is
	// executable.
	scriptPath := filepath.Join(home, FleetManScriptSuffix)
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("hook script not at %s: %v", scriptPath, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("hook script lacks executable bit: %v", info.Mode())
	}

	// Phase 1 / 4 contract: settings.json contains our hook entry
	// for every managed event, with the resolved absolute path.
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	settingsBytes, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	settings := string(settingsBytes)
	for _, event := range fleetManManagedEvents {
		marker := scriptPath + " " + event
		if !strings.Contains(settings, marker) {
			t.Errorf("settings.json missing entry %q\n%s", marker, settings)
		}
	}

	// Phase 2 / 3 / 4 contract: Claude firing each event must
	// produce a state file the detector decodes correctly.
	detector := NewClaudeHookDetector()
	cases := []struct {
		event string
		want  State
	}{
		{"UserPromptSubmit", StateWorking},
		{"PreToolUse", StateWorking},
		{"PostToolUse", StateWorking},
		{"Stop", StateWaiting},
		{"Notification", StateWaiting},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			fireHookAsClaude(t, scriptPath, stateDir, tc.event)

			content, err := os.ReadFile(filepath.Join(stateDir, "claude-state"))
			if err != nil {
				t.Fatalf("read state file: %v", err)
			}

			// Build the synthetic capture the backend would produce.
			// The map key is ClaudeStateFilePath (the production
			// path) — only the contents matter for the detector.
			capture := backend.AllSessions{
				ExtraFiles: map[string]string{
					ClaudeStateFilePath: string(content),
				},
				OK: true,
			}
			got := detector.Detect(capture, time.Now())
			if got != tc.want {
				t.Errorf("event=%s: detector=%d, want=%d (state file: %q)",
					tc.event, got, tc.want, content)
			}
		})
	}
}

// TestEndToEnd_LifecycleTransitions simulates a realistic Claude
// session — UserPromptSubmit → tool calls → Stop, then a second
// turn — and asserts the detector tracks the transitions in real
// time. This is the closest test to "watch fleet-man's TUI report
// the right state as a real conversation happens".
func TestEndToEnd_LifecycleTransitions(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not available: %v", err)
	}

	home := t.TempDir()
	stateDir := t.TempDir()

	if err := NewClaudeProvisioner(&localExecutor{home: home}).Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	scriptPath := filepath.Join(home, FleetManScriptSuffix)

	detector := NewClaudeHookDetector()
	stateAfter := func(event string) State {
		fireHookAsClaude(t, scriptPath, stateDir, event)
		content, err := os.ReadFile(filepath.Join(stateDir, "claude-state"))
		if err != nil {
			t.Fatalf("read state file after %s: %v", event, err)
		}
		return detector.Detect(backend.AllSessions{
			ExtraFiles: map[string]string{ClaudeStateFilePath: string(content)},
			OK:         true,
		}, time.Now())
	}

	// Conversation turn 1: user submits, Claude runs tools, finishes.
	if got := stateAfter("UserPromptSubmit"); got != StateWorking {
		t.Errorf("after UserPromptSubmit: got %d, want StateWorking", got)
	}
	if got := stateAfter("PreToolUse"); got != StateWorking {
		t.Errorf("after PreToolUse: got %d, want StateWorking", got)
	}
	if got := stateAfter("PostToolUse"); got != StateWorking {
		t.Errorf("after PostToolUse: got %d, want StateWorking", got)
	}
	if got := stateAfter("Stop"); got != StateWaiting {
		t.Errorf("after Stop: got %d, want StateWaiting", got)
	}

	// Conversation turn 2: user submits again, Claude pauses for
	// permission (Notification), then completes.
	if got := stateAfter("UserPromptSubmit"); got != StateWorking {
		t.Errorf("turn 2 UserPromptSubmit: got %d, want StateWorking", got)
	}
	if got := stateAfter("Notification"); got != StateWaiting {
		t.Errorf("after Notification (permission prompt): got %d, want StateWaiting", got)
	}
	if got := stateAfter("PostToolUse"); got != StateWorking {
		t.Errorf("after permission granted (PostToolUse): got %d, want StateWorking", got)
	}
	if got := stateAfter("Stop"); got != StateWaiting {
		t.Errorf("turn 2 Stop: got %d, want StateWaiting", got)
	}
}

// TestEndToEnd_ProvisionIdempotent asserts running Provision a
// second time against an already-provisioned directory leaves
// settings.json byte-identical and the hook script unchanged. This
// is what fleet-man relies on to safely re-provision on every
// instance create without risk of file growth or drift.
func TestEndToEnd_ProvisionIdempotent(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not available: %v", err)
	}

	home := t.TempDir()
	executor := &localExecutor{home: home}

	if err := NewClaudeProvisioner(executor).Provision(); err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	scriptPath := filepath.Join(home, FleetManScriptSuffix)

	settingsBefore, _ := os.ReadFile(settingsPath)
	scriptBefore, _ := os.ReadFile(scriptPath)

	if err := NewClaudeProvisioner(executor).Provision(); err != nil {
		t.Fatalf("second Provision: %v", err)
	}

	settingsAfter, _ := os.ReadFile(settingsPath)
	scriptAfter, _ := os.ReadFile(scriptPath)

	if !bytes.Equal(settingsBefore, settingsAfter) {
		t.Errorf("settings.json changed on re-provision\nbefore:\n%s\nafter:\n%s",
			settingsBefore, settingsAfter)
	}
	if !bytes.Equal(scriptBefore, scriptAfter) {
		t.Errorf("hook script changed on re-provision")
	}
}

// fireHookAsClaude exec's the hook script the way Claude Code
// would — pass the event name as argv[1] and pipe a JSON payload
// on stdin.
func fireHookAsClaude(t *testing.T, scriptPath, stateDir, event string) {
	t.Helper()
	cmd := exec.Command(scriptPath, event)
	cmd.Env = append(os.Environ(), "FLEET_MAN_STATE_DIR="+stateDir)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"` + event + `","cwd":"/workspace"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook script failed for event %s: %v\noutput: %s", event, err, out)
	}
}
