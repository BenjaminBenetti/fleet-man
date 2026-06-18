package backend

import "strings"

// sessionMarker is emitted before each session's capture inside
// CaptureAllScript. ASCII RS (\x1e) does not appear in tmux pane
// output, so splitting on this marker is unambiguous.
const sessionMarker = "\x1eFLEET_SESSION_8f4a2b1c\x1e"

// fileMarker is emitted before each captured auxiliary file inside
// CaptureAllScript. Same marker scheme as sessions but a distinct
// payload type so the parser knows which map the body belongs to.
const fileMarker = "\x1eFLEET_FILE_8f4a2b1c\x1e"

// hookMissingMarker is emitted by CaptureAllScript when any fleet-man
// state-detection hook script (Claude Code's or auggie's) is not
// present (or not executable) in the container. The host uses this
// signal to re-install the scripts — without them, the corresponding
// hook detector silently sticks at "waiting" because the agent has no
// way to write its state file.
const hookMissingMarker = "\x1eFLEET_HOOK_MISSING_8f4a2b1c\x1e"

// CaptureAllScript runs every per-container read fleet-man needs in a
// single shell invocation:
//
//  1. List tmux sessions and capture each pane's visible content.
//  2. Cat any /tmp/fleet-man/*-state files written by in-container
//     hook scripts (Claude Code's and auggie's state-hook scripts).
//  3. Verify the state-detection hook scripts (Claude Code's and
//     auggie's) are still present in $HOME, emitting a marker line when
//     either is not so the host can re-provision them. The paths match
//     FleetManScriptSuffix / AuggieScriptSuffix in the agentdetect
//     package; the literals are duplicated here because the backend
//     package cannot import agentdetect.
//
// Each block is preceded by a marker line so the Go side can
// demultiplex back into typed maps. The script is tolerant of
// "nothing to capture" at every step: missing tmux server, missing
// /tmp/fleet-man directory, missing state files all reduce to silent
// no-ops.
const CaptureAllScript = `sessions=$(tmux list-sessions -F "#{session_name}" 2>/dev/null)
if [ -n "$sessions" ]; then
  printf '%s\n' "$sessions" | while IFS= read -r sess; do
    [ -z "$sess" ] && continue
    printf '\036FLEET_SESSION_8f4a2b1c\036%s\n' "$sess"
    tmux capture-pane -t "$sess" -p 2>/dev/null
  done
fi
for f in /tmp/fleet-man/*-state; do
  [ -e "$f" ] || continue
  printf '\036FLEET_FILE_8f4a2b1c\036%s\n' "$f"
  cat "$f" 2>/dev/null || true
done
if [ ! -x "$HOME/.fleet/scripts/claude-state-hook.sh" ] || [ ! -x "$HOME/.fleet/scripts/auggie-state-hook.sh" ]; then
  printf '\036FLEET_HOOK_MISSING_8f4a2b1c\036\n'
fi
`

// ParseCaptureOutput demultiplexes CaptureAllScript output into its
// payload kinds:
//
//   - sessions:    tmux sessionName → ScreenCapture
//   - files:       containerPath    → file contents
//   - hookMissing: true when the hook-script absence marker was
//     present in the output, i.e. a fleet-man hook script (Claude
//     Code's or auggie's) is missing (default false: assume
//     installed unless we have direct evidence otherwise,
//     so a parse with no markers does not trigger
//     unnecessary re-provisioning)
//
// Lines preceding the first marker are ignored (the script may emit
// stray output if tmux is misbehaving and we want to fail soft).
// Returns empty maps when the output has no markers.
func ParseCaptureOutput(output string) (map[string]ScreenCapture, map[string]string, bool) {
	sessions := make(map[string]ScreenCapture)
	files := make(map[string]string)
	hookMissing := false
	if output == "" {
		return sessions, files, hookMissing
	}

	const (
		modeNone = iota
		modeSession
		modeFile
	)

	mode := modeNone
	var currentName string
	var currentBuf strings.Builder
	flush := func() {
		if currentName == "" {
			return
		}
		content := strings.TrimRight(currentBuf.String(), "\n")
		switch mode {
		case modeSession:
			sessions[currentName] = ScreenCapture{Content: content, OK: true}
		case modeFile:
			files[currentName] = content
		}
	}

	for _, line := range strings.SplitAfter(output, "\n") {
		trimmed := strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(trimmed, sessionMarker):
			flush()
			currentName = strings.TrimPrefix(trimmed, sessionMarker)
			mode = modeSession
			currentBuf.Reset()
			continue
		case strings.HasPrefix(trimmed, fileMarker):
			flush()
			currentName = strings.TrimPrefix(trimmed, fileMarker)
			mode = modeFile
			currentBuf.Reset()
			continue
		case strings.HasPrefix(trimmed, hookMissingMarker):
			flush()
			currentName = ""
			mode = modeNone
			hookMissing = true
			continue
		}
		if mode == modeNone {
			continue
		}
		currentBuf.WriteString(line)
	}
	flush()
	return sessions, files, hookMissing
}
