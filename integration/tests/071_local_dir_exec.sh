#!/usr/bin/env bash
# Description: `fleet exec` runs commands directly on the host with cwd set to the workspace dir for local_dir.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
"${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" --backend local_dir

workspace_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/alpha/${FIXTURE_REPO_NAME}"

# Listing a workspace-relative file proves cwd was set correctly.
info "fleet exec alpha -- ls .devcontainer/devcontainer.json"
ls_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- ls .devcontainer/devcontainer.json)
assert_equals ".devcontainer/devcontainer.json" "${ls_out}" "exec cwd should be the workspace dir"

# A simple echo must succeed.
info "fleet exec alpha -- sh -c 'echo hello-from-local'"
hello_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "echo hello-from-local")
assert_equals "hello-from-local" "${hello_out}" "echo output"

# Non-zero exits inside the command must propagate.
info "fleet exec alpha -- sh -c 'exit 7' should fail"
set +e
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "exit 7" >/dev/null 2>&1
rc=$?
set -e
if [ "${rc}" -eq 0 ]; then
  fail "expected non-zero exit from 'exit 7', got 0"
fi

# The command runs on the host (not inside any container): a unique file
# we create on the host in the workspace must be visible to a follow-up
# exec, and a file created via exec must be visible on the host. This
# would be impossible inside an isolated container.
info "host <-> exec roundtrip via shared workspace dir"
echo "host-side" > "${workspace_dir}/.host-marker"
exec_seen=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat .host-marker)
assert_equals "host-side" "${exec_seen}" "exec should see file written by host"

"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "echo exec-side > .exec-marker"
host_seen=$(cat "${workspace_dir}/.exec-marker")
assert_equals "exec-side" "${host_seen}" "host should see file written by exec"

pass "exec (local_dir)"
