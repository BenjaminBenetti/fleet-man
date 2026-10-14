#!/usr/bin/env bash
# itest: k8s
# Description: TUI creates a tmux session inside an instance via the New session dialog
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

info "spawning TUI"
tui_spawn

info "waiting for TUI to hydrate before any keypress"
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

info "moving cursor to alpha instance row"
tui_send j
sleep 0.5

info "expanding the instance session list"
tui_send Space
tui_wait_for "+ new session" 15

info "opening the New session dialog"
tui_send a
tui_wait_for "session-name (or empty for auto)" 5
tui_assert_contains "itest-fleet/alpha" "dialog should show instance key"

info "submitting a session name"
tui_send_text "mysession"
sleep 0.3
tui_send Enter

info "waiting for Session created status"
tui_wait_for "Session created" 15

info "waiting for the new session row to appear"
tui_wait_for "○ mysession" 15
tui_assert_contains "mysession" "new session row missing"

pass "TUI create tmux session inside instance"
