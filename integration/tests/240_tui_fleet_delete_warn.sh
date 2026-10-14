#!/usr/bin/env bash
# itest: k8s
# Description: TUI fleet delete double-confirm warn dialog (cancel + destroy paths)
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "running" 5

# ------------------------------------------------------------
# Path 1: cancel at the warn dialog - fleet + instance survive
# ------------------------------------------------------------
info "opening first-confirm delete dialog"
tui_send d
tui_wait_for "Delete fleet" 5
tui_assert_contains "Remove fleet ${FIXTURE_REPO_NAME}" "first-confirm body missing"

info "pressing y should advance to warn dialog (fleet has running instance)"
tui_send y
tui_wait_for "!! WARNING !!" 5
tui_assert_contains "You are about to destroy fleet ${FIXTURE_REPO_NAME}" "warn dialog body missing"
tui_assert_contains "[y] Confirm destroy" "warn hint missing"

info "pressing n at warn should cancel without deleting"
tui_send n
tui_wait_for "Cancelled" 5
tui_wait_for_absent "!! WARNING !!" 5

tui_assert_contains "alpha" "alpha should still be present after warn-cancel"
tui_assert_contains "running" "alpha should still be running after warn-cancel"

# ------------------------------------------------------------
# Path 2: confirm at warn dialog - fleet + instance destroyed
# ------------------------------------------------------------
info "re-opening first-confirm delete dialog"
tui_send d
tui_wait_for "Delete fleet" 5

info "pressing y advances to warn dialog"
tui_send y
tui_wait_for "!! WARNING !!" 5

info "pressing y at warn dialog destroys fleet + instances"
tui_send y
tui_wait_for "Removed fleet ${FIXTURE_REPO_NAME}" 60
tui_wait_for "No instances" 15

info "verifying via CLI that fleet is gone"
ls_out=$("${FLEET_BIN}" ls || true)
assert_not_contains "${ls_out}" "${FIXTURE_REPO_NAME}" "fleet should not appear in CLI ls after destroy"

pass "TUI fleet delete — warn path cancel + destroy"
