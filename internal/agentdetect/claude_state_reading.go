package agentdetect

import "time"

// ClaudeStateReading is the parsed contents of the Claude state file
// at ClaudeStateFilePath.
type ClaudeStateReading struct {
	// State is the run-state derived from the most recent hook event.
	State State
	// Timestamp is when the hook script wrote the file (truncated to
	// second precision — that is all the wire format carries).
	Timestamp time.Time
}
