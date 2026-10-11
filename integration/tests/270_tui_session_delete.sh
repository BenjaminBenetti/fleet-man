#!/usr/bin/env bash
# itest: k8s
# Description: TUI delete tmux session - exercises both cancel and confirm paths
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn

# Stabilize TUI before any keypress
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

# Navigate to alpha and expand
info "expanding alpha instance"
tui_send j
sleep 0.5
tui_send Space
tui_wait_for "+ new session" 15

# Create session named tokill
info "creating session 'tokill'"
tui_send a
tui_wait_for "session-name (or empty for auto)" 5
tui_send_text "tokill"
sleep 0.3
tui_send Enter
tui_wait_for "Session created" 15
tui_wait_for "tokill" 15

# Move cursor to the session row (was on instance after create)
info "moving cursor to session row"
tui_send j
sleep 0.5

# --- CANCEL PATH ---
info "testing cancel path"
tui_send d
tui_wait_for "Delete session" 5
tui_assert_contains "itest-fleet/alpha" "delete dialog should reference instance"
tui_assert_contains "[y] Yes" "delete dialog hint missing"

tui_send n
tui_wait_for "Cancelled" 5
tui_wait_for_absent "Delete session" 5
tui_assert_contains "tokill" "session should still be present after cancel"

# --- CONFIRM PATH ---
info "testing confirm path"
# Defensively re-settle cursor on the session row
tui_send j
sleep 0.2
tui_send k
sleep 0.2

tui_send d
tui_wait_for "Delete session" 5

tui_send y
tui_wait_for "Deleted session" 15

# Wait for re-render. The status message "Deleted session alpha~tokill"
# contains "tokill" and is not auto-cleared by time — it's only cleared
# on the next keypress that triggers updateNormal. So check for the
# ROW format "○ tokill" (with the session icon) which won't match the
# status message text.
tui_wait_for_absent "○ tokill" 15

pass "TUI delete tmux session — cancel + confirm"
