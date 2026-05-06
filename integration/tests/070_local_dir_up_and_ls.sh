#!/usr/bin/env bash
# Description: `fleet up --backend local_dir` clones the repo, marks the instance running, and creates no docker container.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test

info "fleet up alpha --backend local_dir"
"${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" --backend local_dir

info "fleet ls"
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
printf '%s\n' "${ls_out}"

assert_contains "${ls_out}" "${FIXTURE_REPO_NAME}" "fleet name missing from ls output"
assert_contains "${ls_out}" "alpha"               "instance name missing from ls output"
assert_contains "${ls_out}" "running"             "instance status should be 'running'"

# state.json must exist and record the local_dir backend.
assert_file_exists "${HOME}/.fleet/state.json"
state=$(cat "${HOME}/.fleet/state.json")
assert_contains "${state}" "\"name\": \"alpha\""             "state.json missing instance entry"
assert_contains "${state}" "\"status\": \"running\""         "state.json instance not running"
assert_contains "${state}" "\"backend\": \"local_dir\""      "state.json missing local_dir backend marker"

# The repo should have been cloned to the workspace dir.
workspace_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/alpha/${FIXTURE_REPO_NAME}"
assert_file_exists "${workspace_dir}"
assert_file_exists "${workspace_dir}/.devcontainer/devcontainer.json"

# No docker container should exist for this workspace dir — local_dir is
# container-less. Skip silently if docker isn't on the runner.
if command -v docker >/dev/null 2>&1; then
  containers=$(docker ps -aq --filter "label=devcontainer.local_folder=${workspace_dir}" 2>/dev/null || true)
  if [ -n "${containers}" ]; then
    fail "local_dir instance unexpectedly created docker container(s): ${containers}"
  fi
fi

pass "up + ls (local_dir)"
