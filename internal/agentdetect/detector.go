package agentdetect

import (
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// Detector is the strategy interface for agent run-state detection.
//
// One Detector instance corresponds to one container. Implementations
// own whatever per-container state they need (e.g., previous screen
// captures, last-change timestamps) so the caller does not have to
// thread that state through on every cycle.
//
// A new Detector is built via the factory whenever a container's
// detected agent tool changes (or appears for the first time). When
// the agent disappears the Detector should be discarded so a future
// agent starts from a clean slate.
type Detector interface {
	// Detect inspects a fresh capture and returns the current state
	// for the container. Implementations may mutate internal state
	// to record history for the next call.
	Detect(capture backend.AllSessions, now time.Time) State
}
