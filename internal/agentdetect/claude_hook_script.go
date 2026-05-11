package agentdetect

import _ "embed"

// ===========================================
// Embedded hook script
// ===========================================
//
// fleet-man-state-hook is a tiny POSIX-sh script that Claude Code
// invokes on every lifecycle event we register hooks for. The script
// writes a single line of "<state> <unix-secs>" to ClaudeStateFilePath
// inside the container, which the host then reads via the existing
// container-exec capture path.
//
// The script is embedded into the fleet-man binary at build time so
// there is exactly one source of truth and shipping fleet-man drops
// nothing else on the user's host. Phase 3 / 4 of this feature will
// copy these bytes into the container and chmod them executable.

// ClaudeHookScript is the embedded shell script that runs as the
// command for every fleet-man-managed Claude Code hook entry.
//
//go:embed claude_hook_script.sh
var ClaudeHookScript []byte

// ===========================================
// Container-side paths
// ===========================================

// ClaudeStateDir is the directory inside each container where the
// hook script writes its state file. Must match the default
// STATE_DIR in claude_hook_script.sh — they are the wire contract
// between host and container.
const ClaudeStateDir = "/tmp/fleet-man"

// ClaudeStateFilePath is the absolute path inside each container of
// the single-line state file the hook script writes. The host reads
// this path back to derive the agent's run state.
const ClaudeStateFilePath = ClaudeStateDir + "/claude-state"
