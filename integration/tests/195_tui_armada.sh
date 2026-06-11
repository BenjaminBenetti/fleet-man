#!/usr/bin/env bash
# Description: TUI Armada selector renders in the border and opens via the `A` key
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "running" 5

# The selector is embedded in the list box's top border, defaulting to "local".
tui_assert_contains "Armada [ local ]" "border should carry the Armada selector defaulting to local"

# `A` opens the dropdown (the selector is OUTSIDE the j/k row cycle).
info "opening the armada dropdown with A"
tui_send A
tui_wait_for "Switch armada" 5
tui_assert_contains "local" "dropdown should list the local connection"

# Escape closes it and returns to the fleet list without switching.
info "closing the dropdown with Escape"
tui_send Escape
tui_wait_for "alpha" 5
tui_assert_contains "Armada [ local ]" "still on local after cancelling the dropdown"

pass "TUI armada selector renders and opens via A"
