package agentdetect

import (
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// TestCaptureScriptCoversClaudeStatePath is the cross-package
// contract test: the backend's CaptureAllScript must read from a
// directory that includes the path the hook script writes to. If
// either side is renamed without the other, the integration breaks
// silently — Detect() would always see empty ExtraFiles and
// indefinitely report StateWaiting.
func TestCaptureScriptCoversClaudeStatePath(t *testing.T) {
	if !strings.Contains(backend.CaptureAllScript, ClaudeStateDir) {
		t.Errorf("backend.CaptureAllScript does not reference ClaudeStateDir %q",
			ClaudeStateDir)
	}
	if !strings.HasPrefix(ClaudeStateFilePath, ClaudeStateDir+"/") {
		t.Errorf("ClaudeStateFilePath %q is not under ClaudeStateDir %q",
			ClaudeStateFilePath, ClaudeStateDir)
	}
}

// captureWithFile builds a minimal AllSessions snapshot containing
// only one ExtraFiles entry — enough to exercise the detector
// without entangling tmux-session shape.
func captureWithFile(path, content string) backend.AllSessions {
	return backend.AllSessions{
		ExtraFiles: map[string]string{path: content},
		OK:         true,
	}
}

// TestClaudeHookDetector_DecodesState exercises the round-trip from
// state-file content to the detector's reported State for every
// shape the file can take in the wild.
func TestClaudeHookDetector_DecodesState(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    State
	}{
		{"working", "working 1700000000\n", StateWorking},
		{"waiting", "waiting 1700000000\n", StateWaiting},
		{"empty content (no signal yet)", "", StateWaiting},
		{"unparseable content", "garbled@#$\n", StateWaiting},
		{"unknown sentinel from script", "unknown 100\n", StateWaiting},
		{"future state we don't yet handle", "thinking 100\n", StateWaiting},
		{"missing timestamp", "working\n", StateWaiting},
	}

	detector := NewClaudeHookDetector()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := captureWithFile(ClaudeStateFilePath, tc.content)
			got := detector.Detect(capture, time.Now())
			if got != tc.want {
				t.Errorf("Detect = %d, want %d (content=%q)", got, tc.want, tc.content)
			}
		})
	}
}

// TestClaudeHookDetector_HandlesNilExtraFiles verifies the detector
// safely tolerates a capture where ExtraFiles was never populated —
// the zero-value path before any state file has been captured.
func TestClaudeHookDetector_HandlesNilExtraFiles(t *testing.T) {
	detector := NewClaudeHookDetector()
	capture := backend.AllSessions{OK: true} // ExtraFiles is nil
	if got := detector.Detect(capture, time.Now()); got != StateWaiting {
		t.Errorf("Detect with nil ExtraFiles = %d, want StateWaiting", got)
	}
}

// TestClaudeHookDetector_IgnoresOtherFiles guards against the
// detector accidentally reading from another tool's state file
// (e.g., a future codex-state) and reporting Claude state derived
// from it. Only the Claude-specific path counts.
func TestClaudeHookDetector_IgnoresOtherFiles(t *testing.T) {
	detector := NewClaudeHookDetector()
	capture := backend.AllSessions{
		ExtraFiles: map[string]string{
			"/tmp/fleet-man/codex-state": "working 1700000000",
			"/tmp/some/unrelated":        "waiting 1700000000",
		},
		OK: true,
	}
	if got := detector.Detect(capture, time.Now()); got != StateWaiting {
		t.Errorf("Detect = %d, want StateWaiting (Claude file absent)", got)
	}
}

// TestClaudeHookDetector_StatelessAcrossCalls verifies the detector
// does not retain memory between calls — switching the file
// content between calls flips the state reliably, with no smoothing
// or hysteresis.
func TestClaudeHookDetector_StatelessAcrossCalls(t *testing.T) {
	detector := NewClaudeHookDetector()

	if got := detector.Detect(captureWithFile(ClaudeStateFilePath, "working 100"), time.Now()); got != StateWorking {
		t.Fatalf("first call: got %d, want StateWorking", got)
	}
	if got := detector.Detect(captureWithFile(ClaudeStateFilePath, "waiting 200"), time.Now()); got != StateWaiting {
		t.Fatalf("second call: got %d, want StateWaiting", got)
	}
	if got := detector.Detect(captureWithFile(ClaudeStateFilePath, "working 300"), time.Now()); got != StateWorking {
		t.Fatalf("third call: got %d, want StateWorking", got)
	}
}
