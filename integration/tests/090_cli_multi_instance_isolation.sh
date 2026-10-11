#!/usr/bin/env bash
# itest: k8s
# Description: stop/start/down only affect the named instance; other instances in the same fleet are untouched.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha
fleet_up beta

# Pull both container IDs out of state so we can verify the backend's view of
# each container independently of what fleet says.
alpha_id=$(itest_container_id "${FIXTURE_REPO_NAME}/alpha")
beta_id=$(itest_container_id "${FIXTURE_REPO_NAME}/beta")
[ -n "${alpha_id}" ] || fail "could not resolve alpha container id"
[ -n "${beta_id}"  ] || fail "could not resolve beta container id"
[ "${alpha_id}" != "${beta_id}" ] || fail "alpha and beta share a container id?"
info "alpha=${alpha_id} beta=${beta_id}"

# ===========================================
# Stop alpha — beta must remain running.
# ===========================================
info "fleet stop alpha"
"${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/alpha" >/dev/null

state_alpha=$(itest_container_status "${alpha_id}")
state_beta=$(itest_container_status "${beta_id}")
info "after stop alpha: alpha=${state_alpha} beta=${state_beta}"
assert_container_stopped "${alpha_id}" "alpha should not be running after stop"
assert_container_running "${beta_id}" "stopping alpha must not affect beta"

# ls confirms both rows present, with the right per-row status.
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
printf '%s\n' "${ls_out}"
# Find the per-row line for each instance and check its status column.
alpha_row=$(printf '%s\n' "${ls_out}" | grep -E '^\S+\s+alpha\s' || true)
beta_row=$(printf '%s\n' "${ls_out}" | grep -E '^\S+\s+beta\s'  || true)
[ -n "${alpha_row}" ] || fail "ls missing alpha row: [${ls_out}]"
[ -n "${beta_row}"  ] || fail "ls missing beta row: [${ls_out}]"
assert_contains "${alpha_row}" "stopped" "alpha row should show stopped"
assert_contains "${beta_row}"  "running" "beta row should still show running"

# Beta still works.
echo_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/beta" -- sh -c "echo beta-alive")
assert_equals "beta-alive" "${echo_out}" "beta should still be reachable while alpha is stopped"

# ===========================================
# Start alpha back up — beta untouched.
# ===========================================
info "fleet start alpha"
"${FLEET_BIN}" start "${FIXTURE_REPO_NAME}/alpha" >/dev/null
assert_container_running "${alpha_id}" "alpha should be running again"
assert_container_running "${beta_id}"  "starting alpha must not affect beta"

# ===========================================
# Down beta — alpha must survive in state and on the backend.
# ===========================================
info "fleet down beta"
"${FLEET_BIN}" down "${FIXTURE_REPO_NAME}/beta" >/dev/null

assert_container_absent "${beta_id}" "beta container still exists after down"
assert_container_running "${alpha_id}" "alpha should remain running after beta is removed"

ls_after=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains     "${ls_after}" "alpha" "alpha should remain in ls"
assert_not_contains "${ls_after}" "beta"  "beta should be gone from ls"

# Alpha still functional after sibling teardown.
echo_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "echo alpha-alive")
assert_equals "alpha-alive" "${echo_out}" "alpha should still be reachable after beta down"

pass "stop / start / down target only the named instance"
