#!/usr/bin/env bash
# itest: k8s
# Description: Pressing `d` opens the delete dialog; pressing `n` cancels without removing.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha" 15

info "Move to alpha and open delete dialog"
tui_send j
sleep 0.3
tui_send d
tui_wait_for "Delete instance" 5
tui_assert_contains "Remove itest-fleet/alpha" "delete confirmation body missing"
tui_assert_contains "[y] Yes"                  "delete dialog hint missing"

info "Cancel with n"
tui_send n
tui_wait_for "Cancelled" 5
tui_wait_for_absent "Delete instance" 5

# Instance must still exist after cancel.
tui_assert_contains "alpha"   "instance should still be listed after cancel"
tui_assert_contains "running" "instance should still be running after cancel"

ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_out}" "alpha"   "CLI ls should still list instance after cancel"
assert_contains "${ls_out}" "running" "CLI ls should still show running after cancel"

pass "TUI delete dialog cancel"
