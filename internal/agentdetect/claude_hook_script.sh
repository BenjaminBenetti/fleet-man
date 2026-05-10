#!/bin/sh
# fleet-man-state-hook — Claude Code lifecycle event handler.
#
# Invoked by Claude Code via the hook entries fleet-man installs in
# ~/.claude/settings.json. Writes a single-line "<state> <unix-secs>"
# file that the host reads back via `docker exec cat` to derive the
# activity tracker's per-container view of Claude's run state.
#
# Usage: fleet-man-state-hook <EventName>
#
# Stdin receives a JSON event payload from Claude Code. We drain it
# without parsing because fleet-man only cares which event fired,
# not the per-event details — and not draining risks SIGPIPE on the
# Claude side once the pipe buffer fills.
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
#   UserPromptSubmit / PreToolUse / PostToolUse → working
#       Claude is mid-turn: the user just kicked it off, a tool is
#       about to run, or a tool just finished and Claude is
#       processing the result.
#
#   Notification / Stop → waiting
#       Claude finished its turn (Stop) or paused for user attention
#       (Notification — permission prompts, idle prompts, etc.).
#
# Environment overrides (test/debug only):
#
#   FLEET_MAN_STATE_DIR — alternate state directory. Defaults to
#                        /tmp/fleet-man. The host reads from the
#                        default; overriding it in production will
#                        decouple the writer and reader.

set -eu

# Drain stdin so Claude Code never blocks on a write to our pipe.
cat > /dev/null

EVENT="${1:-unknown}"
STATE_DIR="${FLEET_MAN_STATE_DIR:-/tmp/fleet-man}"
STATE_FILE="${STATE_DIR}/claude-state"

case "$EVENT" in
  UserPromptSubmit|PreToolUse|PostToolUse) STATE="working" ;;
  Notification|Stop)                       STATE="waiting" ;;
  *)                                       STATE="unknown" ;;
esac

NOW=$(date +%s)
mkdir -p "$STATE_DIR"

# Atomic write: same-directory tmp + rename, so a concurrent reader
# never sees a half-written line.
TMP="${STATE_FILE}.tmp.$$"
printf '%s %s\n' "$STATE" "$NOW" > "$TMP"
mv -f "$TMP" "$STATE_FILE"
