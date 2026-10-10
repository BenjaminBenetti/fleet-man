#!/usr/bin/env bash
# Description: `fleet destroy` removes every instance in a fleet and the fleet record itself.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha
fleet_up beta

# Collect container IDs for both instances.
mapfile -t container_ids < <(itest_container_ids)
[ "${#container_ids[@]}" -eq 2 ] || fail "expected 2 container ids in state, got ${#container_ids[@]}"
info "container ids: ${container_ids[*]}"

ls_before=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_before}" "alpha" "alpha missing before destroy"
assert_contains "${ls_before}" "beta"  "beta missing before destroy"

info "fleet destroy ${FIXTURE_REPO_NAME}"
destroy_out=$("${FLEET_BIN}" destroy "${FIXTURE_REPO_NAME}")
printf '%s\n' "${destroy_out}"
assert_contains "${destroy_out}" "destroyed" "destroy should mention destroyed"
assert_contains "${destroy_out}" "2 instances removed" "destroy should remove both instances"

# All containers gone.
for cid in "${container_ids[@]}"; do
  assert_container_absent "${cid}" "container still exists after destroy"
done

# Fleet removed from state.
state=$(cat "${HOME}/.fleet/state.json")
assert_not_contains "${state}" "\"${FIXTURE_REPO_NAME}\"" "fleet should be removed from state"

# `ls` shows no instances from this fleet.
ls_after=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" || true)
assert_not_contains "${ls_after}" "alpha" "alpha should be gone"
assert_not_contains "${ls_after}" "beta"  "beta should be gone"

pass "destroy"
