#!/usr/bin/env bash
# Description: TUI creates a fleet and two instances; verifies header count and both rows
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test

# ===========================================
# Spawn TUI and wait for empty state
# ===========================================
tui_spawn
tui_wait_for "No instances" 15

# ===========================================
# Create the fleet via `n`
# ===========================================
info "Creating fleet via TUI"
tui_send n
tui_wait_for "git@github.com" 5
tui_send_text "${FIXTURE_REPO_URL}"
sleep 0.3
tui_send Enter
tui_wait_for "Added fleet ${FIXTURE_REPO_NAME}" 5
tui_wait_for "${FIXTURE_REPO_NAME}" 5

# Cursor may have landed on the settings row after fleet creation;
# re-seat on the fleet header.
tui_send k
sleep 0.3

# ===========================================
# Create the first instance "alpha"
# ===========================================
info "Creating first instance: alpha"
tui_send a
tui_wait_for "instance-name" 5
tui_send_text "alpha"
sleep 0.3
tui_send Enter

# Devcontainer build can take a while; wait for idle agent indicator.
tui_wait_for "○ idle" 180
tui_assert_contains "alpha" "alpha row missing after create"

# ===========================================
# Create the second instance "beta"
# ===========================================
# Creating the first instance leaves the cursor on the fleet header (it was
# pressed from the header, which stays row 0), so `a` adds another instance to
# the fleet directly. A fixed count of `k` presses no longer reliably re-seats
# now that the Armada selector is a navigation stop above the header.
info "Creating second instance: beta"
tui_send a
tui_wait_for "instance-name" 5
tui_send_text "beta"
sleep 0.3
tui_send Enter

# Creation toast may flash past; allow it to be optional.
tui_wait_for "Creating ${FIXTURE_REPO_NAME}/beta" 10 || true

# beta row should appear quickly in a "creating" state
tui_wait_for "beta" 15

# Both rows should be present while beta hydrates
tui_assert_contains "alpha" "alpha missing after beta create"
tui_assert_contains "beta" "beta row missing"

# The header count updating to (2) is the most reliable signal that
# both instances are registered in fleet state.
tui_wait_for "${FIXTURE_REPO_NAME} (2)" 180

# ===========================================
# Final screen assertions
# ===========================================
tui_assert_contains "${FIXTURE_REPO_NAME} (2)" "fleet header should show count of 2 instances"
tui_assert_contains "alpha" "alpha row missing"
tui_assert_contains "beta" "beta row missing"

# ===========================================
# Verify via CLI as well
# ===========================================
info "Verifying via 'fleet ls'"
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_out}" "alpha" "CLI ls should list alpha"
assert_contains "${ls_out}" "beta" "CLI ls should list beta"

pass "TUI multi-instance fleet (2 instances created via TUI)"
