package agentdetect

import (
	"strconv"
	"strings"
	"time"
)

// ===========================================
// Claude state file parsing
// ===========================================
//
// The hook script writes a single line of the form:
//
//   <state> <unix-secs>\n
//
// where <state> is one of the wire-format strings below. Parsing is
// permissive on the read side: any unrecognised content is treated as
// "no signal" rather than an error, because the file may legitimately
// be empty, partially written (despite the atomic rename — different
// filesystem semantics), or written by a future hook script version
// that emits new state values we do not yet understand.

// ===========================================
// Constants
// ===========================================
//
// These wire-format strings MUST match the values written by
// claude_hook_script.sh. Tests in this package execute the embedded
// script and assert the file content matches, guarding against drift.

const (
	stateValueWorking = "working"
	stateValueWaiting = "waiting"
	stateValueUnknown = "unknown"
)

// ===========================================
// Types
// ===========================================

// ClaudeStateReading is the parsed contents of the Claude state file
// at ClaudeStateFilePath.
type ClaudeStateReading struct {
	// State is the run-state derived from the most recent hook event.
	State State
	// Timestamp is when the hook script wrote the file (truncated to
	// second precision — that is all the wire format carries).
	Timestamp time.Time
}

// ===========================================
// Public API
// ===========================================

// ParseClaudeStateFile parses the content of the Claude state file
// written by the hook script. Returns ok=false (not an error) when
// the content is empty, malformed, or carries an unrecognised state
// value — the caller treats that as "Claude has not yet emitted a
// usable hook signal in this session".
//
// Permissive on purpose: future hook script versions may emit new
// state values, and we never want a forwards-compatibility mismatch
// to surface as an error to the user.
func ParseClaudeStateFile(content string) (ClaudeStateReading, bool) {
	line := firstNonEmptyLine(content)
	if line == "" {
		return ClaudeStateReading{}, false
	}

	stateToken, secsToken, ok := splitStateLine(line)
	if !ok {
		return ClaudeStateReading{}, false
	}

	state, ok := decodeStateToken(stateToken)
	if !ok {
		return ClaudeStateReading{}, false
	}

	secs, err := strconv.ParseInt(secsToken, 10, 64)
	if err != nil || secs < 0 {
		return ClaudeStateReading{}, false
	}

	return ClaudeStateReading{
		State:     state,
		Timestamp: time.Unix(secs, 0),
	}, true
}

// ===========================================
// Private helpers
// ===========================================

// firstNonEmptyLine returns the first non-empty, trimmed line from
// content. Returns "" when content has no usable line. Tolerates
// trailing newlines, leading whitespace, and stray blank lines so a
// future hook script that decides to add a header line still
// produces parseable output.
func firstNonEmptyLine(content string) string {
	for raw := range strings.SplitSeq(content, "\n") {
		if line := strings.TrimSpace(raw); line != "" {
			return line
		}
	}
	return ""
}

// splitStateLine splits a "<state> <secs>" line into its two
// whitespace-separated tokens. Returns ok=false when the line does
// not have at least two tokens; trailing tokens beyond the second
// are silently ignored to leave room for forwards-compatible
// extensions.
func splitStateLine(line string) (state, secs string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", false
	}
	return fields[0], fields[1], true
}

// decodeStateToken maps the wire-format state string to the
// agentdetect.State enum. The explicit "unknown" sentinel the hook
// script writes for unrecognised events is matched by name (rather
// than falling into default) to make the contract with the script
// readable in one glance, and to ensure renaming the constant
// trips the test that scans the embedded script for it.
func decodeStateToken(token string) (State, bool) {
	switch token {
	case stateValueWorking:
		return StateWorking, true
	case stateValueWaiting:
		return StateWaiting, true
	case stateValueUnknown:
		return StateNotRunning, false
	default:
		return StateNotRunning, false
	}
}
