package agentdetect

import (
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// ===========================================
// AuggieHookDetector
// ===========================================

// AuggieHookDetector derives auggie's run state from the state file the
// in-container auggie-state-hook script writes on every lifecycle event
// we register (SessionStart, PromptSubmit, PreToolUse, PostToolUse,
// Notification, Stop).
//
// It is the Augment CLI peer of ClaudeHookDetector and shares the same
// "<state> <unix-secs>" wire format, so it reuses ParseClaudeStateFile
// to decode the file. Stateless by design: every Detect call inspects
// only the latest state-file contents from the capture.
//
// Fallback semantics for the "no signal" cases (file absent, empty, or
// unparseable) match Claude's: the activity tracker only routes
// captures here after the agent-tool probe confirms auggie is alive, so
// an empty state file means "auggie is running but has not emitted a
// hook event yet" — the safe display is StateWaiting (auggie exists,
// idle) rather than StateNotRunning (which would contradict the probe).
type AuggieHookDetector struct{}

// ===========================================
// Constructors
// ===========================================

// NewAuggieHookDetector returns a fresh detector instance. The type
// holds no state; a constructor is provided for symmetry with the rest
// of the package.
func NewAuggieHookDetector() *AuggieHookDetector {
	return &AuggieHookDetector{}
}

// ===========================================
// Detector implementation
// ===========================================

// Detect reads the auggie state file out of the capture's ExtraFiles
// and decodes it. The time argument is unused — the hook script stamps
// each transition; the detector reports the most recent value.
func (d *AuggieHookDetector) Detect(capture backend.AllSessions, _ time.Time) State {
	content := capture.ExtraFiles[AuggieStateFilePath]
	reading, ok := ParseClaudeStateFile(content)
	if !ok {
		return StateWaiting
	}
	return reading.State
}
