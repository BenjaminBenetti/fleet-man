package backend

// ListSessionsScript lists the live tmux sessions in the
// "name:windows:attached" format the runtime session poller parses. tmux
// exits non-zero when no server is running; 2>/dev/null swallows that so the
// caller simply observes empty output ("no sessions") rather than an error.
const ListSessionsScript = `tmux list-sessions -F "#{session_name}:#{session_windows}:#{session_attached}" 2>/dev/null`

// AllSessions holds the per-container snapshot collected in a single
// shell invocation: tmux pane captures, plus auxiliary state files
// produced by tool-specific in-container hooks (Claude Code's
// fleet-man-state-hook is the first consumer).
//
//   - OK=false means the exec itself failed (transient — preserve prev state)
//   - OK=true with empty Sessions means tmux has no sessions (agent not running)
//   - OK=true with populated Sessions means each named session has a capture
//   - ExtraFiles maps absolute container path → file contents for any
//     /tmp/fleet-man/*-state files present at capture time. Detectors
//     read their own state file by path; missing entries simply mean
//     the file did not exist.
//   - ClaudeHookMissing is true only when CaptureAllScript explicitly
//     observed that the Claude state-detection hook script is gone
//     from the container. Default false means "no signal to act on" —
//     a transient capture failure does NOT trigger re-provisioning.
type AllSessions struct {
	Sessions          map[string]ScreenCapture // sessionName → capture
	ExtraFiles        map[string]string        // containerPath → contents
	ClaudeHookMissing bool
	OK                bool
}
