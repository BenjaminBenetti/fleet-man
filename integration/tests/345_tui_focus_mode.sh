#!/usr/bin/env bash
# Description: TUI 'f' enters focus mode — the tall banner collapses to the compact
# "Fleet" logo and the "settings" row becomes "[ leave focus ]"; esc, q, and the
# leave-focus row all exit focus (focus behaves like a dialog for q/esc).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha"  15
tui_wait_for "○ idle" 60

# ===========================================
# Baseline: the tall ASCII banner and the "settings" row are present.
# ===========================================
tui_assert_contains "|_| |_" "tall fleet banner should be visible before focus"
tui_assert_contains "settings" "settings row should be visible before focus"

# ===========================================
# 'f' on the fleet enters focus mode: the banner collapses (the tall
# banner's distinctive bottom row disappears) and "settings" becomes
# "[ leave focus ]".
# ===========================================
info "pressing 'f' to enter focus mode"
tui_send f
tui_wait_for "leave focus" 5
tui_assert_not_contains "settings" "settings row should be hidden in focus mode"
tui_assert_not_contains "|_| |_" "tall banner should be replaced by the compact logo in focus mode"

# ===========================================
# esc leaves focus (dialog-like) and restores the normal page.
# ===========================================
info "pressing esc to leave focus"
tui_send Escape
tui_wait_for "settings" 5
tui_wait_for_absent "leave focus" 5
tui_assert_contains "|_| |_" "tall banner should return after leaving focus"

# ===========================================
# 'q' leaves focus too — it must NOT quit the TUI.
# ===========================================
info "re-entering focus, then 'q' to leave (must not quit)"
tui_send f
tui_wait_for "leave focus" 5
tui_send q
tui_wait_for "settings" 5
# The TUI is still alive: the instance row is still on screen.
tui_assert_contains "alpha" "TUI must stay running after 'q' left focus"

# ===========================================
# The "[ leave focus ]" row + Enter exits focus. With one instance the
# rows are: fleet header, instance, [ leave focus ] — two 'j' reach it.
# ===========================================
info "re-entering focus, selecting [ leave focus ] with Enter"
tui_send f
tui_wait_for "leave focus" 5
tui_send j
sleep 0.2
tui_send j
sleep 0.2
tui_send Enter
tui_wait_for "settings" 5
tui_wait_for_absent "leave focus" 5

pass "TUI focus mode: enter via 'f', leave via esc / q / [ leave focus ]"
