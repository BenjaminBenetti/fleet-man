#!/usr/bin/env bash
# itest: k8s
# Description: TUI edit instance — rename via display_name through the edit dialog, verify persistence
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "running" 5

# Move cursor to alpha instance row
tui_send j
sleep 0.3

# Open the edit dialog via 'e' — cursor lands on the Name row with the
# current display name prefilled.
tui_send e
tui_wait_for "Edit instance" 5
tui_assert_contains "itest-fleet" "edit dialog should show fleet name"

# Clear the prefilled name by sending backspaces, then type a new one.
for _ in 1 2 3 4 5; do tui_send BSpace; done
tui_send_text "auth-fix"
sleep 0.3
tui_send Enter

# Expect the dialog to close and the new label to appear in the list.
tui_wait_for_absent "Edit instance" 5
tui_wait_for "auth-fix" 5

# Verify DisplayName persisted in state.json and the underlying Name is untouched.
display_value=$(grep -oE '"display_name"[[:space:]]*:[[:space:]]*"[^"]*"' "${HOME}/.fleet/state.json" | head -1 || true)
assert_contains "${display_value}" "auth-fix" "display_name should persist in state.json after save"
name_value=$(grep -oE '"name"[[:space:]]*:[[:space:]]*"alpha"' "${HOME}/.fleet/state.json" | head -1 || true)
assert_contains "${name_value}" "alpha" "underlying name should remain unchanged after rename"

pass "TUI edit instance — rename via display_name"
