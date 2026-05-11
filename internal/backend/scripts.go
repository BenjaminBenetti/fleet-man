package backend

import "strings"

// ===========================================
// Tool Probe
// ===========================================

// ToolProbeScript detects which agent tool (if any) is running inside
// a container. It runs a single `ps` scan, excludes the probe's own
// process group to avoid self-matching, and checks every command-line
// field against all tool names at once. Priority order: claude,
// copilot, codex, gemini.
//
// The matching pattern (^|/)t($|[-./]) recognises:
//   - standalone binaries: `claude`, `/usr/local/bin/claude`
//   - interpreter-wrapped CLIs: `node /opt/copilot-cli/cli.js`
//     (`copilot` appears as a path segment)
//   - versioned paths: `gemini-cli`, `copilot.js`
//
// It rejects substrings like `my-claude-notes.txt` because the tool
// name must start a path component, which avoids false positives from
// files being edited or viewed.
const ToolProbeScript = `MY_PGID=$(ps -o pgid= -p $$ 2>/dev/null | tr -d ' \t\n\r')
ps -eo pid,pgid,args --no-headers 2>/dev/null | awk -v pgid="$MY_PGID" '
BEGIN {
  n = 4
  tools[1] = "claude"
  tools[2] = "copilot"
  tools[3] = "codex"
  tools[4] = "gemini"
}
$2 == pgid { next }
{
  for (i = 3; i <= NF; i++) {
    for (j = 1; j <= n; j++) {
      if ($i ~ "(^|/)"tools[j]"($|[-./])") {
        found[j] = 1
      }
    }
  }
}
END {
  for (j = 1; j <= n; j++) {
    if (found[j]) { print tools[j]; exit 0 }
  }
  print "-"
}
'
`

// ParseToolProbeOutput parses the stdout of ToolProbeScript. The
// second return is false only when the probe exec failed (empty
// output). Empty tool with ok=true means the probe ran but found no
// agent.
func ParseToolProbeOutput(output string) (string, bool) {
	tool := strings.TrimSpace(output)
	if tool == "" {
		return "", false
	}
	if tool == "-" {
		return "", true
	}
	return tool, true
}

// ===========================================
// Capture (sessions + per-tool state files)
// ===========================================

// sessionMarker is emitted before each session's capture inside
// CaptureAllScript. ASCII RS (\x1e) does not appear in tmux pane
// output, so splitting on this marker is unambiguous.
const sessionMarker = "\x1eFLEET_SESSION_8f4a2b1c\x1e"

// fileMarker is emitted before each captured auxiliary file inside
// CaptureAllScript. Same marker scheme as sessions but a distinct
// payload type so the parser knows which map the body belongs to.
const fileMarker = "\x1eFLEET_FILE_8f4a2b1c\x1e"

// hookMissingMarker is emitted by CaptureAllScript when the Claude
// state-detection hook script is not present (or not executable) in
// the container. The host uses this signal to re-install the script
// — without it, fleet-man's Claude detector silently sticks at
// "waiting" because Claude has no way to write the state file.
const hookMissingMarker = "\x1eFLEET_HOOK_MISSING_8f4a2b1c\x1e"

// CaptureAllScript runs every per-container read fleet-man needs in a
// single shell invocation:
//
//  1. List tmux sessions and capture each pane's visible content.
//  2. Cat any /tmp/fleet-man/*-state files written by in-container
//     hook scripts (today: Claude Code's fleet-man-state-hook).
//  3. Verify the Claude state-detection hook script is still present
//     in $HOME, emitting a marker line when it is not so the host can
//     re-provision it. The path matches FleetManScriptSuffix in the
//     agentdetect package; the literal is duplicated here because the
//     backend package cannot import agentdetect.
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
if [ ! -x "$HOME/.fleet/scripts/claude-state-hook.sh" ]; then
  printf '\036FLEET_HOOK_MISSING_8f4a2b1c\036\n'
fi
`

// ParseCaptureOutput demultiplexes CaptureAllScript output into its
// payload kinds:
//
//   - sessions:    tmux sessionName → ScreenCapture
//   - files:       containerPath    → file contents
//   - hookMissing: true when the Claude hook-script absence marker
//                  was present in the output (default false: assume
//                  installed unless we have direct evidence otherwise,
//                  so a parse with no markers does not trigger
//                  unnecessary re-provisioning)
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
