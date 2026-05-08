#!/usr/bin/env bash
# Description: TUI 'f' opens the port-forward dialog on running instances, validates input, and closes on esc; refuses on stopped instances.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha"  15
tui_wait_for "○ idle" 60

# ===========================================
# 'f' on the fleet header has no instance selected — defensive UX
# prints "Select an instance" instead of opening the dialog.
# ===========================================
info "pressing 'f' on fleet header (no instance selected)"
tui_send f
tui_wait_for "Select an instance" 5
# Make sure the dialog did NOT open.
tui_assert_not_contains "local:remote" "port-forward dialog should not open without an instance"

# ===========================================
# Move cursor to alpha and press 'f' to open the dialog.
# ===========================================
info "moving cursor to alpha"
tui_send j
sleep 0.3

info "pressing 'f' to open port-forward dialog"
tui_send f
tui_wait_for "local:remote" 5

# ===========================================
# Bad input — the dialog handler routes through parsePortMapping and
# surfaces the error in m.message. Type "abc" (no colon) and submit.
# ===========================================
info "submitting invalid mapping 'abc'"
tui_send_text "abc"
sleep 0.3
tui_send Enter
tui_wait_for "expected local:remote" 5

# Bad port number — type "0:80" (local out of range).
# Clear the input first via repeated backspace.
for _ in 1 2 3 4 5; do tui_send BSpace; done
info "submitting invalid local port '0:80'"
tui_send_text "0:80"
sleep 0.3
tui_send Enter
tui_wait_for "invalid local port" 5

# ===========================================
# Esc closes the dialog without adding any forward.
# ===========================================
info "pressing Esc to close dialog"
tui_send Escape
tui_wait_for_absent "local:remote" 5
# Row should not have a "⇄" port-forward indicator (none was added).
tui_assert_not_contains "⇄" "no port forwards should be active after esc"

# ===========================================
# Stop the instance via 's' and verify 'f' refuses to open the dialog
# with a state-gate message.
# ===========================================
info "stopping alpha via 's'"
tui_send s
tui_wait_for "Stopped itest-fleet/alpha" 20
tui_wait_for "stopped" 5

info "pressing 'f' on a stopped instance"
tui_send f
tui_wait_for "Instance must be running to port-forward" 5
tui_assert_not_contains "local:remote" "port-forward dialog should not open on stopped instance"

pass "TUI 'f' port-forward dialog gating + validation"
