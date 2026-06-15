#!/usr/bin/env bash
# Description: `fleet rebuild` recreates the container (new id) while preserving the workspace, and the instance stays usable.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

old_container=$(grep -oE '"container_id":\s*"[^"]+"' "${HOME}/.fleet/state.json" | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
[ -n "${old_container}" ] || fail "could not read container_id from state"
info "first container: ${old_container}"

# Drop a marker into the workspace (a host bind mount), so we can prove the
# rebuild preserves the checkout rather than wiping it like a destroy would.
ws_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/alpha/${FIXTURE_REPO_NAME}"
echo "preserved-through-rebuild" > "${ws_dir}/REBUILD_MARKER.txt"

# --- rebuild ---
info "fleet rebuild alpha"
rebuild_out=$("${FLEET_BIN}" rebuild "${FIXTURE_REPO_NAME}/alpha")
printf '%s\n' "${rebuild_out}"
assert_contains "${rebuild_out}" "rebuilt" "rebuild output should mention rebuilt"

# The container is torn down and recreated, so its id changes...
new_container=$(grep -oE '"container_id":\s*"[^"]+"' "${HOME}/.fleet/state.json" | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
[ -n "${new_container}" ] || fail "could not read new container_id from state"
info "new container: ${new_container}"
if [ "${old_container}" = "${new_container}" ]; then
  fail "container id should change after rebuild (container was not recreated)"
fi

# ...and the old container is gone.
if docker inspect "${old_container}" >/dev/null 2>&1; then
  fail "old container ${old_container} should have been removed by rebuild"
fi

# The fresh container is running.
docker_state=$(docker inspect -f '{{.State.Status}}' "${new_container}")
assert_equals "running" "${docker_state}" "rebuilt container should be running"

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
