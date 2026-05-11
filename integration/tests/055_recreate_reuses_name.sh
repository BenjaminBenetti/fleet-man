#!/usr/bin/env bash
# Description: `fleet up` with a reused name prunes any stale container left behind, so `fleet exec` does not hit runc's mount-namespace check (#51).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

old_container=$(grep -oE '"container_id":\s*"[^"]+"' "${HOME}/.fleet/state.json" | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
[ -n "${old_container}" ] || fail "could not read container_id from state"
ws_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/alpha/${FIXTURE_REPO_NAME}"
info "first container: ${old_container}"

# Sanity check: the container is labelled with the workspace path the
# devcontainer CLI uses to match it. This is the same label the fix
# queries, so if this assertion fails the test is no longer covering
# the right thing.
label=$(docker inspect --format '{{ index .Config.Labels "devcontainer.local_folder" }}' "${old_container}")
assert_equals "${ws_dir}" "${label}" "container should carry the devcontainer.local_folder label"

# Simulate a failed Down: drop fleet's state + workspace clone but leave
# the docker container behind. This matches the failure path that
# triggers issue #51 — the next `fleet up` finds a container under the
# expected label and would otherwise silently reuse it.
info "wiping fleet state but leaving container ${old_container} alive"
rm -rf "${HOME}/.fleet/state.json" "${HOME}/.fleet/workspaces"
mkdir -p "${HOME}/.fleet"
if ! docker inspect "${old_container}" >/dev/null 2>&1; then
  fail "precondition failed: stale container ${old_container} should still exist"
fi

info "fleet up alpha again — pre-prune should drop ${old_container} and provision a fresh container"
fleet_up alpha

new_container=$(grep -oE '"container_id":\s*"[^"]+"' "${HOME}/.fleet/state.json" | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
[ -n "${new_container}" ] || fail "could not read new container_id from state"
info "new container: ${new_container}"

if [ "${old_container}" = "${new_container}" ]; then
  fail "new container should be different from the stale one — pre-prune did not run"
fi

if docker inspect "${old_container}" >/dev/null 2>&1; then
  fail "stale container ${old_container} should have been pruned"
fi

# Final regression check: `fleet exec` must succeed against the fresh
# container. Pre-fix this is the call that surfaced runc's "current
# working directory is outside of container mount namespace root"
# error, because devcontainer was reusing the stale container instead
# of creating a fresh one.
info "fleet exec alpha -- uname -s"
uname_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- uname -s)
assert_equals "Linux" "${uname_out}" "exec inside fresh container"

pass "stale container pruned on instance re-create"
