#!/usr/bin/env bash
# itest: k8s
# Description: Pressing space on the fleet header collapses / expands the instance list.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha" 15
# Cursor starts on the fleet header. Expanded marker = ▼, collapsed = ▶.
tui_assert_contains "▼ itest-fleet" "fleet should start expanded"
tui_assert_contains "alpha"         "instance should be visible when expanded"

info "Collapse with space"
tui_send Space
tui_wait_for "▶ itest-fleet" 5

# alpha should disappear from the screen while collapsed.
tui_wait_for_absent "alpha" 5 || fail "instance still visible after collapse"

info "Expand with space again"
tui_send Space
tui_wait_for "▼ itest-fleet" 5
tui_wait_for "alpha" 5

pass "TUI collapse / expand via space"
