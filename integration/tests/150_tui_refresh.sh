#!/usr/bin/env bash
# itest: k8s
# Description: Pressing `r` refreshes state and surfaces new instances created via the CLI.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha" 15
tui_assert_not_contains "beta" "beta should not exist yet"

info "Create 'beta' via CLI while the TUI is running"
fleet_up beta >/dev/null

# Initially the TUI has not re-read state.json. Press `r` to refresh.
info "Press 'r' to refresh"
tui_send r
tui_wait_for "Refreshed" 5
tui_wait_for "beta" 5

pass "TUI refresh picks up new instance"
