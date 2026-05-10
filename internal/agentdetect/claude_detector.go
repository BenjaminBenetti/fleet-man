package agentdetect

import (
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// ===========================================
// ClaudeHookDetector
// ===========================================

// ClaudeHookDetector derives Claude Code's run state from the state
// file the in-container fleet-man-state-hook script writes on every
// hook event we register (UserPromptSubmit, PreToolUse, PostToolUse,
// Notification, Stop).
//
// Stateless by design: every Detect call inspects only the latest
// state-file contents from the capture. The hook script is the
// source of truth — fleet-man does not try to second-guess Claude's
// own lifecycle.
//
// Fallback semantics for the "no signal" cases (file absent, empty,
// or unparseable):
//
//   The activity tracker only routes captures to a detector after
//   the agent-tool probe has confirmed Claude is alive. Reaching
//   Detect with an empty state file therefore means "Claude is
//   running but has not yet emitted a hook event" — fresh
//   container, hook script not yet provisioned (pre-Phase-4), or
//   fresh session with no UserPromptSubmit yet. In all of those the
//   safe display is StateWaiting (Claude exists, idle from our
//   perspective) rather than StateNotRunning (which would
//   contradict the probe).
type ClaudeHookDetector struct{}

// ===========================================
// Constructors
// ===========================================

// NewClaudeHookDetector returns a fresh detector instance. The type
// holds no state, but a constructor is provided for symmetry with
// the rest of the package and to leave room for future per-instance
// configuration (e.g., an alternate state file path for tests).
func NewClaudeHookDetector() *ClaudeHookDetector {
	return &ClaudeHookDetector{}
}

// ===========================================
// Detector implementation
// ===========================================

// Detect reads the Claude state file out of the capture's ExtraFiles
// and decodes it. The time argument is unused — the hook script
// already stamps each transition; the detector simply reports the
// most recent value.
func (d *ClaudeHookDetector) Detect(capture backend.AllSessions, _ time.Time) State {
	content := capture.ExtraFiles[ClaudeStateFilePath]
	reading, ok := ParseClaudeStateFile(content)
	if !ok {
		return StateWaiting
	}
	return reading.State
}
