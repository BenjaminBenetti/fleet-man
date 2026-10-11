#!/usr/bin/env bash
# itest: k8s
# Description: TUI tag instance — save tag via Enter, cancel via Esc, verify persistence
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

# Open the tag dialog
tui_send t
tui_wait_for "Tag instance" 5
tui_assert_contains "itest-fleet/alpha" "tag dialog should show instance key"

# Type a tag and submit
tui_send_text "important"
sleep 0.3
tui_send Enter

# Expect success message and dialog close
tui_wait_for "Tagged itest-fleet/alpha: important" 5
tui_wait_for_absent "Tag instance" 5

# Re-open dialog to test cancel; defensively re-settle cursor on alpha
tui_send j
sleep 0.2
tui_send k
sleep 0.2
tui_send t
tui_wait_for "Tag instance" 5

# Cancel with Escape
tui_send Escape
tui_wait_for "Cancelled" 5
tui_wait_for_absent "Tag instance" 5

# Verify tag persisted in state.json
tag_value=$(grep -oE '"tag"[[:space:]]*:[[:space:]]*"[^"]*"' "${HOME}/.fleet/state.json" | head -1 || true)
assert_contains "${tag_value}" "important" "tag should persist in state.json after save"

pass "TUI tag instance — save + cancel"
