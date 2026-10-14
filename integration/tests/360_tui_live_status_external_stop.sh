#!/usr/bin/env bash
# itest: k8s
# Description: TUI startup live-status probe reconciles drift when the container was stopped externally.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

# Pull the container ID out of state.json.
container_id=$(itest_container_id "${FIXTURE_REPO_NAME}/alpha")
[ -n "${container_id}" ] || fail "could not read container_id from state.json"
info "container_id = ${container_id}"

# Sanity-check: fleet currently records running.
assert_contains "$(cat "${HOME}/.fleet/state.json")" '"status": "running"' \
  "expected initial state to be running"

info "Stop the container behind fleet's back to simulate an inactivity timeout / crash"
itest_container_stop "${container_id}"

# Nothing has reconciled the drift yet, so the persisted status is stale.
assert_contains "$(cat "${HOME}/.fleet/state.json")" '"status": "running"' \
  "state file should still claim running before TUI starts"

info "Launch the TUI — its startup live-status probe should flip the row to stopped"
tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "stopped" 30

# The flip must also have been written through to disk so a future
# fleet invocation sees the corrected status.
sleep 1
assert_contains "$(cat "${HOME}/.fleet/state.json")" '"status": "stopped"' \
  "state file should reflect the corrected status after the startup probe"

pass "TUI startup probe detects external container stop"
