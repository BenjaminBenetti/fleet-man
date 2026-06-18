package agentdetect

import (
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// TestCaptureScriptCoversAuggieStatePath is the cross-package contract
// test for auggie: the auggie state file must live under the directory
// the backend's CaptureAllScript scans (the /tmp/fleet-man/*-state
// glob), otherwise Detect() would always see empty ExtraFiles and
// indefinitely report StateWaiting.
func TestCaptureScriptCoversAuggieStatePath(t *testing.T) {
	if !strings.Contains(backend.CaptureAllScript, ClaudeStateDir) {
		t.Errorf("backend.CaptureAllScript does not reference the state dir %q", ClaudeStateDir)
	}
	if !strings.HasPrefix(AuggieStateFilePath, ClaudeStateDir+"/") {
		t.Errorf("AuggieStateFilePath %q is not under the state dir %q",
			AuggieStateFilePath, ClaudeStateDir)
	}
	if !strings.HasSuffix(AuggieStateFilePath, "-state") {
		t.Errorf("AuggieStateFilePath %q does not end in -state, so the *-state glob misses it",
			AuggieStateFilePath)
	}
}

// TestAuggieHookDetector_DecodesState exercises the round-trip from
// state-file content to the detector's reported State for every shape
// the file can take in the wild. The fallback for "no signal" content
// is StateWaiting, matching Claude.
func TestAuggieHookDetector_DecodesState(t *testing.T) {
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
		{"missing timestamp", "working\n", StateWaiting},
	}

	detector := NewAuggieHookDetector()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := captureWithFile(AuggieStateFilePath, tc.content)
			got := detector.Detect(capture, time.Now())
			if got != tc.want {
				t.Errorf("Detect = %d, want %d (content=%q)", got, tc.want, tc.content)
			}
		})
	}
}

// TestAuggieHookDetector_HandlesNilExtraFiles verifies the detector
// tolerates a capture where ExtraFiles was never populated.
func TestAuggieHookDetector_HandlesNilExtraFiles(t *testing.T) {
	detector := NewAuggieHookDetector()
	capture := backend.AllSessions{OK: true} // ExtraFiles is nil
	if got := detector.Detect(capture, time.Now()); got != StateWaiting {
		t.Errorf("Detect with nil ExtraFiles = %d, want StateWaiting", got)
	}
}

// TestAuggieHookDetector_IgnoresOtherFiles guards against the detector
// reading another tool's state file. Only the auggie-specific path
// counts — a Claude state file present must not leak into auggie state.
func TestAuggieHookDetector_IgnoresOtherFiles(t *testing.T) {
	detector := NewAuggieHookDetector()
	capture := backend.AllSessions{
		ExtraFiles: map[string]string{
			ClaudeStateFilePath:          "working 1700000000",
			"/tmp/fleet-man/codex-state": "working 1700000000",
		},
		OK: true,
	}
	if got := detector.Detect(capture, time.Now()); got != StateWaiting {
		t.Errorf("Detect = %d, want StateWaiting (auggie file absent)", got)
	}
}
