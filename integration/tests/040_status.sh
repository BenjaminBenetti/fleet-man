#!/usr/bin/env bash
# itest: k8s
# Description: `fleet status` reports fleet and instance running/stopped counts.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test

# Empty state.
info "fleet status (no fleets)"
empty_out=$("${FLEET_BIN}" status)
assert_contains "${empty_out}" "No fleets" "empty status should say 'No fleets'"

# One running instance.
fleet_up alpha
info "fleet status (1 running)"
s1=$("${FLEET_BIN}" status)
printf '%s\n' "${s1}"
assert_contains "${s1}" "${FIXTURE_REPO_NAME}" "fleet name missing"
assert_contains "${s1}" "1 running" "should report 1 running"

# Stop it — counts should shift.
"${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/alpha" >/dev/null
info "fleet status (1 stopped)"
s2=$("${FLEET_BIN}" status)
printf '%s\n' "${s2}"
assert_contains "${s2}" "0 running" "should report 0 running"
assert_contains "${s2}" "1 stopped" "should report 1 stopped"

pass "status"
