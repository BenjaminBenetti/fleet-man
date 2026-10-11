#!/usr/bin/env bash
# itest: k8s
# Description: TUI settings page opens from fleet list and closes with esc and q
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
# Wait for a fully-hydrated row (○ idle agent indicator), not just "alpha".
# The initial render can show "alpha" before buildRows/stats settle, and
# keystrokes landing during that window are unreliable.
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

tui_assert_contains "settings" "settings row missing"

# Verify the cursor is on the fleet header by toggling collapse. Space on
# the header flips the ▼/▶ arrow; if cursor were elsewhere Space would do
# something else entirely. This also protects us against any transient
# cursor clamping during TUI startup.
info "verifying cursor is on the fleet header via Space-collapse toggle"
tui_assert_contains "▼ itest-fleet" "fleet should start expanded"
tui_send Space
tui_wait_for "▶ itest-fleet" 5
tui_send Space
tui_wait_for "▼ itest-fleet" 5

# From the header, two 'j' presses walk deterministically down through
# rows [header, alpha, settings] → cursor on settings.
info "moving cursor to the settings row with j j"
tui_send j
sleep 0.5
tui_send j
sleep 0.5

info "opening Settings with Enter"
tui_send Enter
tui_wait_for "Tmux vim keys" 10
tui_assert_contains "General" "General section missing"
tui_assert_contains "Show help text" "Show help text row missing"

info "closing Settings with Escape"
tui_send Escape
tui_wait_for "alpha" 5
tui_wait_for_absent "Tmux vim keys" 5

# On return from Settings, cursor remains on the settings row (buildRows
# preserves wasOnSettings). Pressing Enter re-opens the page directly.
info "re-opening Settings (cursor remains on settings row)"
tui_send Enter
tui_wait_for "Tmux vim keys" 10

info "closing Settings with q"
tui_send q
tui_wait_for "alpha" 5
tui_wait_for_absent "Tmux vim keys" 5

pass "TUI settings page open + close (esc and q)"
