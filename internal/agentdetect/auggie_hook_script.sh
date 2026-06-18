#!/bin/sh
# fleet-man-state-hook (auggie) — Augment CLI lifecycle event handler.
#
# Invoked by auggie via the hook entries fleet-man installs in
# ~/.augment/settings.json. Writes a single-line "<state> <unix-secs>"
# file that the host reads back via the capture path to derive the
# activity tracker's per-container view of auggie's run state.
#
# auggie identifies the firing event two ways (we prefer the first):
#
#   1. The AUGMENT_HOOK_EVENT environment variable, set on every hook
#      invocation. This is robust regardless of how auggie spawns the
#      command (shell vs execFile).
#   2. The first positional argument, which we also wire into each
#      hook entry's "args" as a belt-and-suspenders fallback.
#
# Stdin receives a JSON event payload from auggie. We drain it without
# parsing because fleet-man only cares which event fired, not the
# per-event details — and not draining risks SIGPIPE on the auggie
# side once the pipe buffer fills.
#
# Output file format:
#
#   <state> <unix-secs>\n
#
#   state    one of: working, waiting, unknown
#   secs     seconds since epoch when this event was processed
#
# State mapping rationale:
#
#   PromptSubmit / PreToolUse / PostToolUse → working
#       auggie is mid-turn: the user just kicked it off, a tool is
#       about to run, or a tool just finished and auggie is
#       processing the result.
#
#   SessionStart / Notification / Stop → waiting
#       auggie just launched and is idle (SessionStart), finished its
#       turn (Stop), or paused for user attention (Notification).
#
# Environment overrides (test/debug only):
#
#   FLEET_MAN_STATE_DIR — alternate state directory. Defaults to
#                        /tmp/fleet-man. The host reads from the
#                        default; overriding it in production will
#                        decouple the writer and reader.

set -eu

# Drain stdin so auggie never blocks on a write to our pipe.
cat > /dev/null

EVENT="${AUGMENT_HOOK_EVENT:-${1:-unknown}}"
STATE_DIR="${FLEET_MAN_STATE_DIR:-/tmp/fleet-man}"
STATE_FILE="${STATE_DIR}/auggie-state"

case "$EVENT" in
  PromptSubmit|PreToolUse|PostToolUse) STATE="working" ;;
  SessionStart|Notification|Stop)      STATE="waiting" ;;
  *)                                   STATE="unknown" ;;
esac

NOW=$(date +%s)
mkdir -p "$STATE_DIR"

# Atomic write: same-directory tmp + rename, so a concurrent reader
# never sees a half-written line.
TMP="${STATE_FILE}.tmp.$$"
printf '%s %s\n' "$STATE" "$NOW" > "$TMP"
mv -f "$TMP" "$STATE_FILE"
