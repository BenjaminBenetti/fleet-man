#!/usr/bin/env bash
# Description: TUI j/k navigation cycles through rows and the Armada selector, wrapping
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "running" 5

# Cursor starts on the fleet header — confirm via Space toggling collapse.
tui_assert_contains "▼ itest-fleet" "fleet should start expanded"
info "collapsing fleet to confirm cursor starts on header"
tui_send Space
tui_wait_for "▶ itest-fleet" 5
info "re-expanding fleet"
tui_send Space
tui_wait_for "▼ itest-fleet" 5

# Up from the top row focuses the Armada selector (a virtual stop above the
# list); enter opens its dropdown.
info "k from the header focuses the Armada selector"
tui_send k
sleep 0.2
tui_send Enter
tui_wait_for "Switch armada" 5
info "Armada dropdown opened from the selector"
tui_send Escape
tui_wait_for "alpha" 5

# From the focused selector, k wraps down to the bottom (settings) row; enter
# opens the settings page.
info "k from the selector wraps to the bottom row (settings)"
tui_send k
sleep 0.2
tui_send Enter
tui_wait_for "Tmux vim keys" 5
info "wrapped to the settings row (settings page rendered)"
tui_send Escape
tui_wait_for "alpha" 5

pass "TUI j/k cycles through rows and the Armada selector with wrap"
