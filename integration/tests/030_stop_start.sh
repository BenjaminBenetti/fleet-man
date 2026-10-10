#!/usr/bin/env bash
# Description: Stop/start preserves container identity and runs postStart once per start (#229).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

# Grab the container ID from state so we can independently verify the
# backend's view of the container.
container_id=$(itest_container_id "${FIXTURE_REPO_NAME}/alpha")
[ -n "${container_id}" ] || fail "could not read container_id from state.json"
info "container id: ${container_id}"

# The fixture devcontainer.json appends a line to ~/.fleet-poststart.log every
# time postStartCommand runs. Counting the lines lets us prove the devcontainer
# postStart hook fires on each start — the regression behind issue #179.
count_poststart() {
  "${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- \
    sh -c 'cat "$HOME/.fleet-poststart.log" 2>/dev/null | wc -l | tr -d " "'
}

# postStart runs once as part of the initial `up`.
poststart_create=$(count_poststart)
info "poststart count after create: ${poststart_create}"
assert_equals "1" "${poststart_create}" "postStart should run once during create"

# --- stop ---
info "fleet stop alpha"
stop_out=$("${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/alpha")
printf '%s\n' "${stop_out}"
assert_contains "${stop_out}" "stopped" "stop output should mention stopped"

# The backend reports the container as stopped (docker: exited/created), not
# running and not gone.
container_state=$(itest_container_status "${container_id}")
info "container state after stop: ${container_state}"
assert_container_stopped "${container_id}" "expected container to not be running after stop"

ls_after_stop=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_after_stop}" "stopped" "ls should report stopped status"

# Second stop is a no-op.
info "fleet stop alpha (already stopped)"
stop_again=$("${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/alpha")
assert_contains "${stop_again}" "already stopped" "repeated stop should be a no-op"

# --- start ---
info "fleet start alpha"
start_out=$("${FLEET_BIN}" start "${FIXTURE_REPO_NAME}/alpha")
printf '%s\n' "${start_out}"
assert_contains "${start_out}" "started" "start output should mention started"

assert_container_running "${container_id}" "container should be running after start"

ls_after_start=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_after_start}" "running" "ls should report running status"

# exec works again after start.
echo_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "echo back-online")
assert_equals "back-online" "${echo_out}" "exec after start"

# Issue #179: starting a stopped instance must re-run the devcontainer
# postStartCommand, just like every other devcontainer tool. The marker now
# holds a second line.
poststart_restart=$(count_poststart)
info "poststart count after restart: ${poststart_restart}"
assert_equals "2" "${poststart_restart}" "postStart must re-run when the instance is started (#179)"

# An already-running start must not repeat lifecycle hooks.
start_again=$("${FLEET_BIN}" start "${FIXTURE_REPO_NAME}/alpha")
assert_contains "${start_again}" "already running" "repeated start should be a no-op"
assert_equals "2" "$(count_poststart)" "already-running start must not repeat postStart"

# A second real restart proves the hook is not a one-time resume action.
"${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/alpha"
"${FLEET_BIN}" start "${FIXTURE_REPO_NAME}/alpha"
assert_equals "3" "$(count_poststart)" "postStart must run on every restart (#229)"
assert_container_running "${container_id}" "restart should preserve the same container"

pass "stop + start lifecycle hooks"
