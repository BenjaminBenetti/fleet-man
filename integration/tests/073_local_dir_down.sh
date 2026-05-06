#!/usr/bin/env bash
# Description: `fleet down` on a local_dir instance removes the workspace dir and state entry without touching docker.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
"${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" --backend local_dir

workspace_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/alpha/${FIXTURE_REPO_NAME}"
assert_file_exists "${workspace_dir}"

info "fleet down alpha"
down_out=$("${FLEET_BIN}" down "${FIXTURE_REPO_NAME}/alpha")
printf '%s\n' "${down_out}"
assert_contains "${down_out}" "removed" "down should mention removed"

# Workspace clone gone.
assert_file_absent "${workspace_dir}"

# State no longer has the instance.
state=$(cat "${HOME}/.fleet/state.json")
assert_not_contains "${state}" "\"name\": \"alpha\"" "instance should be removed from state"

# `ls` no longer lists it.
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_not_contains "${ls_out}" "alpha" "ls should not list removed instance"

pass "down (local_dir)"
