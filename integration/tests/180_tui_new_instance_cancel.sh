#!/usr/bin/env bash
# Description: TUI 'a' on fleet header opens new-instance dialog; 'esc' cancels without creating instance
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

info "spawning TUI"
tui_spawn
tui_wait_for "alpha" 15

info "pressing 'a' on fleet header to open new-instance dialog"
tui_send a
tui_wait_for "instance-name" 5
tui_assert_contains "New instance" "new-instance dialog title missing"

info "pressing Ctrl+c to cancel the dialog (Name field auto-focuses on open; ctrl+c is the documented cancel from active mode)"
tui_send "C-c"
tui_wait_for_absent "instance-name" 5
tui_wait_for_absent "New instance" 5

info "verifying original alpha instance is still intact"
tui_assert_contains "alpha" "alpha should still be visible after cancel"
tui_assert_contains "running" "alpha should still be running after cancel"

info "verifying via CLI ls that no spurious instance was created"
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_out}" "alpha" "CLI ls should still list alpha after cancel"
assert_not_contains "${ls_out}" "instance-name" "CLI ls should not contain placeholder 'instance-name' (no stray instance)"

pass "TUI new-instance dialog cancel (esc)"
