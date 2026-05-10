#!/usr/bin/env bash
# Description: TUI rename tmux session (esc-cancel + rename end-to-end)
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

info "spawning TUI"
tui_spawn

# Stabilize render before sending keys to avoid keypress-during-hydration races.
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

info "navigating to alpha and expanding"
tui_send j
sleep 0.5
tui_send Space
tui_wait_for "+ new session" 15

info "creating session 'origname'"
tui_send a
tui_wait_for "session-name (or empty for auto)" 5
tui_send_text "origname"
sleep 0.3
tui_send Enter
tui_wait_for "Session created" 15
tui_wait_for "origname" 15

info "moving cursor to the session row"
# After create, cursor is still on the alpha instance row.
# Rows now: [header, alpha, origname-row, new-session, settings].
tui_send j
sleep 0.5

info "opening rename dialog"
tui_send r
tui_wait_for "Rename session" 5
tui_assert_contains "Current:" "rename dialog missing Current label"
tui_assert_contains "origname" "rename dialog should reference original name"

info "testing ctrl+c cancel on rename dialog (name field auto-focuses on open)"
tui_send "C-c"
tui_wait_for "Cancelled" 5
tui_wait_for_absent "Rename session" 5
tui_assert_contains "origname" "session should still exist after cancel"

info "re-opening rename dialog (defensively re-settle cursor)"
tui_send j
sleep 0.2
tui_send k
sleep 0.2
tui_send r
tui_wait_for "Rename session" 5

info "submitting new name 'renamed'"
tui_send_text "renamed"
sleep 0.3
tui_send Enter

# renameGroupCmd emits "Renamed session alpha~origname → alpha~renamed"
tui_wait_for "Renamed session" 15
tui_wait_for "renamed" 15

info "verifying renamed session appears on screen"
tui_assert_contains "renamed" "renamed session row missing"
# Note: we intentionally do NOT assert absence of "origname" because the
# status message "Renamed session alpha~origname → alpha~renamed" contains it.

pass "TUI rename tmux session"
