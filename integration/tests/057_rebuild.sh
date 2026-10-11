#!/usr/bin/env bash
# itest: k8s
# Description: `fleet rebuild` recreates the container (new id) while preserving the workspace, and the instance stays usable.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

inst="${FIXTURE_REPO_NAME}/alpha"
old_container=$(itest_container_id "${inst}")
[ -n "${old_container}" ] || fail "could not read container_id from state"
info "first container: ${old_container}"

# Drop a marker into the workspace (devcontainer: the host bind mount), so we
# can prove the rebuild preserves the checkout rather than wiping it like a
# destroy would.
itest_workspace_write "${inst}" REBUILD_MARKER.txt "preserved-through-rebuild"

# --- rebuild ---
info "fleet rebuild alpha"
rebuild_out=$("${FLEET_BIN}" rebuild "${FIXTURE_REPO_NAME}/alpha")
printf '%s\n' "${rebuild_out}"
assert_contains "${rebuild_out}" "rebuilt" "rebuild output should mention rebuilt"

# The container is torn down and recreated, so its id changes...
new_container=$(itest_container_id "${inst}")
[ -n "${new_container}" ] || fail "could not read new container_id from state"
info "new container: ${new_container}"
if [ "${old_container}" = "${new_container}" ]; then
  fail "container id should change after rebuild (container was not recreated)"
fi

# ...and the old container is gone.
assert_container_absent "${old_container}" "old container should have been removed by rebuild"

# The fresh container is running.
assert_container_running "${new_container}" "rebuilt container should be running"

# State reports running again (rebuilding -> running).
ls_after=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_after}" "running" "ls should report running after rebuild"

# The workspace survived: the marker is readable from inside the new container.
marker_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat "/workspaces/${FIXTURE_REPO_NAME}/REBUILD_MARKER.txt")
assert_equals "preserved-through-rebuild" "${marker_out}" "workspace should be preserved through rebuild"

# exec works against the rebuilt container.
echo_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "echo back-online")
assert_equals "back-online" "${echo_out}" "exec after rebuild"

pass "rebuild recreates container, preserves workspace"
