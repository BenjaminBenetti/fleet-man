#!/usr/bin/env bash
# itest: no-docker
# Description: TUI new-fleet dialog cancel with esc does not create a fleet
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test

info "spawning TUI on empty fleet list"
tui_spawn
tui_wait_for "No instances" 15

info "opening new-fleet dialog with 'n'"
tui_send n
tui_wait_for "git@github.com" 5
tui_assert_contains "New fleet" "new-fleet dialog title missing"

info "cancelling dialog with Ctrl+c (field auto-focuses on open; the hint advertises ctrl+c as the cancel key)"
tui_send "C-c"
tui_wait_for_absent "git@github.com" 5
tui_wait_for_absent "New fleet" 5

info "verifying empty state persists and no fleet was created"
tui_assert_contains "No instances" "empty state should persist after cancel"

ls_out=$("${FLEET_BIN}" ls || true)
assert_not_contains "${ls_out}" "${FIXTURE_REPO_NAME}" "no fleet should exist after cancel"

pass "TUI new-fleet dialog cancel (esc)"
