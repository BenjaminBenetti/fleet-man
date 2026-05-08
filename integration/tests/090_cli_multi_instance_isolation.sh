#!/usr/bin/env bash
# Description: stop/start/down only affect the named instance; other instances in the same fleet are untouched.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha
fleet_up beta

# Pull both container IDs out of state so we can verify docker-level state
# independently of what fleet says. The state file is small JSON pretty-
# printed by Go's encoder — `name` always appears in the same object as
# its sibling `container_id`, so awk can walk to the next container_id
# after each `name` line.
state_file="${HOME}/.fleet/state.json"
extract_container_id() {
  local instance_name="$1"
  awk -v want="\"name\": \"${instance_name}\"" '
    index($0, want)        { found=1; next }
    found && /"container_id"/ { print; exit }
  ' "${state_file}" | grep -oE '"[a-f0-9]{12,}"' | tr -d '"' | head -1
}
alpha_id=$(extract_container_id alpha)
beta_id=$(extract_container_id beta)
[ -n "${alpha_id}" ] || fail "could not resolve alpha container id"
[ -n "${beta_id}"  ] || fail "could not resolve beta container id"
[ "${alpha_id}" != "${beta_id}" ] || fail "alpha and beta share a container id?"
info "alpha=${alpha_id} beta=${beta_id}"

# ===========================================
# Stop alpha — beta must remain running.
# ===========================================
info "fleet stop alpha"
"${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/alpha" >/dev/null

state_alpha=$(docker inspect -f '{{.State.Status}}' "${alpha_id}")
state_beta=$(docker inspect -f '{{.State.Status}}' "${beta_id}")
info "after stop alpha: alpha=${state_alpha} beta=${state_beta}"
if [ "${state_alpha}" = "running" ]; then
  fail "alpha should not be running after stop"
fi
assert_equals "running" "${state_beta}" "stopping alpha must not affect beta"

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
state_alpha=$(docker inspect -f '{{.State.Status}}' "${alpha_id}")
state_beta=$(docker inspect -f '{{.State.Status}}' "${beta_id}")
assert_equals "running" "${state_alpha}" "alpha should be running again"
assert_equals "running" "${state_beta}"  "starting alpha must not affect beta"

# ===========================================
# Down beta — alpha must survive in state and on docker.
# ===========================================
info "fleet down beta"
"${FLEET_BIN}" down "${FIXTURE_REPO_NAME}/beta" >/dev/null

if docker inspect "${beta_id}" >/dev/null 2>&1; then
  fail "beta container ${beta_id} still exists after down"
fi
state_alpha=$(docker inspect -f '{{.State.Status}}' "${alpha_id}")
assert_equals "running" "${state_alpha}" "alpha should remain running after beta is removed"

ls_after=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains     "${ls_after}" "alpha" "alpha should remain in ls"
assert_not_contains "${ls_after}" "beta"  "beta should be gone from ls"

# Alpha still functional after sibling teardown.
echo_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "echo alpha-alive")
assert_equals "alpha-alive" "${echo_out}" "alpha should still be reachable after beta down"

pass "stop / start / down target only the named instance"
