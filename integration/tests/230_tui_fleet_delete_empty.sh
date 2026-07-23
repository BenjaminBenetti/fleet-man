#!/usr/bin/env bash
# itest: no-docker
# Description: TUI delete on empty fleet uses single-confirm path (no warn dialog)
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test

tui_spawn
tui_wait_for "No instances" 15

# Create an empty fleet via TUI (no instances added)
tui_send n
tui_wait_for "git@github.com" 5
tui_send_text "${FIXTURE_REPO_URL}"
sleep 0.3
tui_send Enter
tui_wait_for "Added fleet ${FIXTURE_REPO_NAME}" 5
tui_wait_for "${FIXTURE_REPO_NAME} (0)" 5

# Move cursor back to the fleet header (may be on settings row after creation)
tui_send k
sleep 0.3

# Open delete dialog
tui_send d
tui_wait_for "Delete fleet" 5
tui_assert_contains "[y] Yes" "delete dialog hint missing"
tui_assert_contains "Remove fleet ${FIXTURE_REPO_NAME}" "delete body missing fleet name"

# Confirm with y — empty fleet should take the single-confirm path
tui_send y
tui_wait_for "Removed fleet ${FIXTURE_REPO_NAME}" 5
tui_assert_not_contains "!! WARNING !!" "empty-fleet delete should not show warn dialog"

# Fleet should be gone — the status message contains the fleet name, so assert empty state instead
tui_wait_for "No instances" 10

# Verify via CLI
ls_out=$("${FLEET_BIN}" ls || true)
assert_not_contains "${ls_out}" "${FIXTURE_REPO_NAME}" "fleet should be absent from CLI ls after delete"

pass "TUI delete empty fleet (single confirm)"
