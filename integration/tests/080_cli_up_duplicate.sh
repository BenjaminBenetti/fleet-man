#!/usr/bin/env bash
# Description: `fleet up` rejects creating an instance with a name that already exists in the fleet.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

# Snapshot the container ID so we can assert the second `up` did not
# silently replace it with a fresh container.
container_id_before=$(grep -oE '"container_id":\s*"[^"]+"' "${HOME}/.fleet/state.json" \
  | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
[ -n "${container_id_before}" ] || fail "could not read container_id from state.json"
info "container id before second up: ${container_id_before}"

# ===========================================
# Second `fleet up` for the same name must error and not create a new
# container, not duplicate the state entry, and not nuke the existing
# instance.
# ===========================================
info "fleet up alpha (already exists, should fail)"
set +e
dup_out=$("${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" 2>&1)
dup_rc=$?
set -e
printf '%s\n' "${dup_out}"

if [ "${dup_rc}" -eq 0 ]; then
  fail "duplicate up should fail, got rc=0; out=[${dup_out}]"
fi
assert_contains "${dup_out}" "already exists" "duplicate up should mention 'already exists'"
assert_contains "${dup_out}" "alpha"           "duplicate up should mention instance name"

# ===========================================
# State must still reflect the original instance. The container ID must
# not have changed and there must be exactly one alpha entry.
# ===========================================
container_id_after=$(grep -oE '"container_id":\s*"[^"]+"' "${HOME}/.fleet/state.json" \
  | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
assert_equals "${container_id_before}" "${container_id_after}" \
  "container ID changed after rejected duplicate up"

alpha_count=$(grep -cE '"name":\s*"alpha"' "${HOME}/.fleet/state.json" || true)
assert_equals "1" "${alpha_count}" "expected exactly one alpha entry in state.json"

# Original container is still usable — nothing was torn down by the
# rejected duplicate.
info "fleet exec alpha -- echo still-alive"
echo_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "echo still-alive")
assert_equals "still-alive" "${echo_out}" "exec on alpha after rejected duplicate"

# ===========================================
# A different instance name in the same fleet must succeed — the rejection
# is per-instance, not per-fleet.
# ===========================================
info "fleet up beta (new name, same fleet, should succeed)"
fleet_up beta
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_out}" "alpha" "alpha should still be present"
assert_contains "${ls_out}" "beta"  "beta should now be present"

pass "fleet up rejects duplicate instance names"
