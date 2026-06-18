package agentdetect

import _ "embed"

// ===========================================
// Embedded hook script
// ===========================================
//
// auggie-state-hook is the Augment CLI counterpart to the Claude hook
// script. auggie invokes it on every lifecycle event we register a
// hook for; the script writes a single line of "<state> <unix-secs>"
// to AuggieStateFilePath inside the container, which the host then
// reads via the existing capture path (the /tmp/fleet-man/*-state
// glob in backend.CaptureAllScript picks it up automatically).
//
// The script is embedded into the fleet-man binary at build time so
// there is exactly one source of truth and shipping fleet-man drops
// nothing else on the user's host.

// AuggieHookScript is the embedded shell script that runs as the
// command for every fleet-man-managed auggie hook entry.
//
//go:embed auggie_hook_script.sh
var AuggieHookScript []byte

// ===========================================
// Container-side paths
// ===========================================

// AuggieStateFilePath is the absolute path inside each container of
// the single-line state file the auggie hook script writes. It shares
// the fleet-man state directory (ClaudeStateDir == /tmp/fleet-man)
// with the other hook-based detectors so the capture script's
// /tmp/fleet-man/*-state glob captures it without per-tool wiring. The
// host reads this path back to derive auggie's run state.
const AuggieStateFilePath = ClaudeStateDir + "/auggie-state"
