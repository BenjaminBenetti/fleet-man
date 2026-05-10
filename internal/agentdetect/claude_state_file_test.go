package agentdetect

import (
	"strings"
	"testing"
	"time"
)

// TestParseClaudeStateFile covers the parser against every shape the
// state file can take in the wild: well-formed lines, empty content,
// partial/malformed lines, unknown tokens, trailing newlines, and a
// forward-compatible scenario where the file carries extra trailing
// fields a future hook script version might add.
func TestParseClaudeStateFile(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		wantOK    bool
		wantState State
		wantSecs  int64
	}{
		{
			name:      "well-formed working",
			content:   "working 1234567890\n",
			wantOK:    true,
			wantState: StateWorking,
			wantSecs:  1234567890,
		},
		{
			name:      "well-formed waiting",
			content:   "waiting 1717000000\n",
			wantOK:    true,
			wantState: StateWaiting,
			wantSecs:  1717000000,
		},
		{
			name:    "no trailing newline still parses",
			content: "working 42",
			wantOK:  true, wantState: StateWorking, wantSecs: 42,
		},
		{
			name:    "leading whitespace tolerated",
			content: "   working 100\n",
			wantOK:  true, wantState: StateWorking, wantSecs: 100,
		},
		{
			name:    "extra fields ignored (forward compatible)",
			content: "waiting 500 future-field-we-do-not-yet-know\n",
			wantOK:  true, wantState: StateWaiting, wantSecs: 500,
		},
		{
			name:    "second line ignored",
			content: "working 9\nspurious extra line\n",
			wantOK:  true, wantState: StateWorking, wantSecs: 9,
		},
		{
			name:    "blank lines skipped",
			content: "\n\nwaiting 1\n",
			wantOK:  true, wantState: StateWaiting, wantSecs: 1,
		},
		{name: "empty string", content: "", wantOK: false},
		{name: "whitespace only", content: "   \n\t\n", wantOK: false},
		{name: "missing timestamp", content: "working\n", wantOK: false},
		{name: "non-numeric timestamp", content: "working notanumber\n", wantOK: false},
		{name: "negative timestamp", content: "working -1\n", wantOK: false},
		{name: "unknown sentinel", content: "unknown 100\n", wantOK: false},
		{name: "unrecognised future state", content: "thinking 100\n", wantOK: false},
		{name: "garbled content", content: "@#$ %^&\n", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseClaudeStateFile(tc.content)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.wantOK, got)
			}
			if !ok {
				return
			}
			if got.State != tc.wantState {
				t.Errorf("state = %d, want %d", got.State, tc.wantState)
			}
			if got.Timestamp.Unix() != tc.wantSecs {
				t.Errorf("timestamp = %d, want %d", got.Timestamp.Unix(), tc.wantSecs)
			}
		})
	}
}

// TestParseClaudeStateFile_TimestampHasSecondPrecision verifies the
// parser produces a time.Time that round-trips cleanly to the unix
// second value the hook script wrote — sub-second precision is not
// part of the wire format, so the Time should land exactly on a
// second boundary in UTC.
func TestParseClaudeStateFile_TimestampHasSecondPrecision(t *testing.T) {
	got, ok := ParseClaudeStateFile("working 1234567890\n")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.Timestamp.Nanosecond() != 0 {
		t.Errorf("timestamp has sub-second component: %v", got.Timestamp)
	}
	if got.Timestamp.Unix() != 1234567890 {
		t.Errorf("timestamp drifted: %d", got.Timestamp.Unix())
	}
}

// TestClaudeHookScript_Embedded verifies the script bytes are
// actually embedded into the binary and contain the wire-format
// strings the parser expects. This is the canary for "someone
// edited the .sh without telling the parser".
func TestClaudeHookScript_Embedded(t *testing.T) {
	if len(ClaudeHookScript) == 0 {
		t.Fatal("ClaudeHookScript is empty — go:embed directive broken?")
	}
	body := string(ClaudeHookScript)
	for _, marker := range []string{
		stateValueWorking,
		stateValueWaiting,
		stateValueUnknown,
		"UserPromptSubmit",
		"PreToolUse",
		"PostToolUse",
		"Notification",
		"Stop",
		"FLEET_MAN_STATE_DIR",
		ClaudeStateDir,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("embedded script missing required marker %q", marker)
		}
	}
}

// TestClaudeStateFilePath_DerivedFromDir locks the relationship
// between the state directory and the state file path: tests and
// callers depend on the file living directly inside the directory.
func TestClaudeStateFilePath_DerivedFromDir(t *testing.T) {
	if !strings.HasPrefix(ClaudeStateFilePath, ClaudeStateDir+"/") {
		t.Errorf("ClaudeStateFilePath %q does not live under ClaudeStateDir %q",
			ClaudeStateFilePath, ClaudeStateDir)
	}
}

// TestParseClaudeStateFile_TimestampUsesUnixOrigin sanity-checks the
// parser interprets the field as unix seconds, not some other
// epoch — a 5-second-old timestamp from "now" should round-trip
// within a few seconds.
func TestParseClaudeStateFile_TimestampUsesUnixOrigin(t *testing.T) {
	now := time.Now().Unix()
	got, ok := ParseClaudeStateFile("working " + itoa(now-5) + "\n")
	if !ok {
		t.Fatal("parse failed")
	}
	delta := time.Since(got.Timestamp)
	if delta < 4*time.Second || delta > 6*time.Second {
		t.Errorf("time delta = %v, want ~5s — parser may be using wrong epoch", delta)
	}
}

// itoa avoids pulling strconv into the test wire just for one int
// formatting call.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
