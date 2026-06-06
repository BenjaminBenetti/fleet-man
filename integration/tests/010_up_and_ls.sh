#!/usr/bin/env bash
# Description: `fleet up` creates a running container and `fleet ls` reports it.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test

info "fleet up alpha"
fleet_up alpha

info "fleet ls"
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
printf '%s\n' "${ls_out}"

assert_contains "${ls_out}" "${FIXTURE_REPO_NAME}" "fleet name missing from ls output"
assert_contains "${ls_out}" "alpha"               "instance name missing from ls output"
assert_contains "${ls_out}" "running"             "instance status should be 'running'"

# state.json must exist and list the instance.
assert_file_exists "${HOME}/.fleet/state.json"
state=$(cat "${HOME}/.fleet/state.json")
assert_contains "${state}" "\"name\": \"alpha\""  "state.json missing instance entry"
assert_contains "${state}" "\"status\": \"running\"" "state.json instance not running"

pass "up + ls"
