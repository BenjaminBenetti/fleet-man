#!/usr/bin/env bash
# Description: `fleet start` on an already-running instance is a no-op (mirror of stop's already-stopped path).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

# Container should already be running after `fleet up`.
container_id=$(itest_container_id "${FIXTURE_REPO_NAME}/alpha")
[ -n "${container_id}" ] || fail "could not read container_id from state.json"

assert_container_running "${container_id}" "container should be running after fleet up"

# ===========================================
# `fleet start` while running — should succeed (rc=0) and surface the
# already-running message rather than error.
# ===========================================
info "fleet start alpha (already running)"
start_out=$("${FLEET_BIN}" start "${FIXTURE_REPO_NAME}/alpha")
printf '%s\n' "${start_out}"
assert_contains "${start_out}" "already running" "start on running instance should report already-running"

# Container is still the SAME container — the no-op must not have replaced it.
assert_container_running "${container_id}" "container should remain running after no-op start"

new_container_id=$(itest_container_id "${FIXTURE_REPO_NAME}/alpha")
assert_equals "${container_id}" "${new_container_id}" \
  "container ID should not change after no-op start"

# ===========================================
# Real round-trip: stop → start → stop on already-stopped → start on running
# exercises both no-op branches in instanceops without re-pulling the image.
# ===========================================
info "fleet stop alpha"
"${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/alpha" >/dev/null
assert_container_stopped "${container_id}" "container should not be running after stop"

info "fleet start alpha (was stopped)"
start_out=$("${FLEET_BIN}" start "${FIXTURE_REPO_NAME}/alpha")
assert_contains "${start_out}" "started" "start after stop should report started"
assert_container_running "${container_id}" "container should be running after start"

info "fleet start alpha (still running — second no-op)"
start_again=$("${FLEET_BIN}" start "${FIXTURE_REPO_NAME}/alpha")
assert_contains "${start_again}" "already running" "second start should still be a no-op"

pass "fleet start no-op on already-running instance"
