package agentdetect

import (
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// ===========================================
// Constants
// ===========================================

// screenChangeThreshold is the minimum number of characters that must
// differ between consecutive screen captures to count as meaningful
// activity (catches spinner animations while ignoring cursor blink).
const screenChangeThreshold = 3

// screenActivityWindow is how recently a meaningful screen change must
// have occurred for the agent to be considered actively working.
const screenActivityWindow = 12 * time.Second

// ===========================================
// TmuxPaneChangeDetector
// ===========================================

// TmuxPaneChangeDetector implements Detector by diffing consecutive
// tmux pane captures. It is the generic fallback that works for any
// agent tool that runs inside a tmux session: if the visible screen
// keeps changing, the agent is working; if it has been static for
// longer than screenActivityWindow, the agent is waiting.
//
// The detector is per-container and tracks each tmux session inside
// it independently — activity in any session marks the container as
// working.
type TmuxPaneChangeDetector struct {
	// ===========================================
	// Fields
	// ===========================================

	prevScreen map[string]string    // sessionName → last captured content
	lastChange map[string]time.Time // sessionName → timestamp of last meaningful change
}

// ===========================================
// Constructors
// ===========================================

// NewTmuxPaneChangeDetector returns an initialised detector with no
// prior screen history.
func NewTmuxPaneChangeDetector() *TmuxPaneChangeDetector {
	return &TmuxPaneChangeDetector{
		prevScreen: make(map[string]string),
		lastChange: make(map[string]time.Time),
	}
}

// ===========================================
// Public Methods
// ===========================================

// Detect compares each session's current content against the last
// captured content for that session. Behaviour:
//
//   - capture.OK=true with no sessions → StateNotRunning
//   - any session changed by ≥ screenChangeThreshold within
//     screenActivityWindow → StateWorking
//   - sessions exist but none changed → StateWaiting
//   - first call (no history yet) → StateWaiting
//
// The OK=false (exec failure) case is intentionally NOT handled here
// — the tracker preserves prior state across transient failures
// before consulting the detector.
func (d *TmuxPaneChangeDetector) Detect(capture backend.AllSessions, now time.Time) State {
	if len(capture.Sessions) == 0 {
		return StateNotRunning
	}

	nextPrev := make(map[string]string, len(capture.Sessions))
	nextLastChange := make(map[string]time.Time, len(capture.Sessions))
	anyHadHistory := false
	workingDetected := false

	for sessionName, sessionCapture := range capture.Sessions {
		if !sessionCapture.OK {
			continue
		}
		prev, hasPrev := d.prevScreen[sessionName]
		lastChange := d.lastChange[sessionName]
		if hasPrev {
			anyHadHistory = true
			if countDiffs(prev, sessionCapture.Content) >= screenChangeThreshold {
				lastChange = now
			}
		}
		nextPrev[sessionName] = sessionCapture.Content
		nextLastChange[sessionName] = lastChange
		if !lastChange.IsZero() && now.Sub(lastChange) < screenActivityWindow {
			workingDetected = true
		}
	}

	d.prevScreen = nextPrev
	d.lastChange = nextLastChange

	switch {
	case workingDetected:
		return StateWorking
	case anyHadHistory:
		return StateWaiting
	default:
		// First capture(s) — no history yet, assume waiting.
		return StateWaiting
	}
}

// ===========================================
// Helpers
// ===========================================

// countDiffs returns the number of character positions that differ
// between two strings, plus any length difference.
func countDiffs(oldContent, newContent string) int {
	diffs := 0
	oldRunes, newRunes := []rune(oldContent), []rune(newContent)
	minLen := min(len(oldRunes), len(newRunes))
	for i := range minLen {
		if oldRunes[i] != newRunes[i] {
			diffs++
		}
	}
	if len(oldRunes) > minLen {
		diffs += len(oldRunes) - minLen
	} else {
		diffs += len(newRunes) - minLen
	}
	return diffs
}
