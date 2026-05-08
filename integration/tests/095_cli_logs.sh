#!/usr/bin/env bash
# Description: `fleet logs` streams the container's docker logs output, including the entrypoint banner.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

container_id=$(grep -oE '"container_id":\s*"[^"]+"' "${HOME}/.fleet/state.json" \
  | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
[ -n "${container_id}" ] || fail "could not read container_id from state.json"

# ===========================================
# 1. `fleet logs` (non-follow) returns the container's stdout. The
#    fixture's entrypoint runs `echo Container started`, so that
#    sentinel should appear. We compare against `docker logs` directly
#    to make sure the two outputs agree — that's the contract.
# ===========================================
info "fleet logs alpha (non-follow)"
fleet_logs=$("${FLEET_BIN}" logs "${FIXTURE_REPO_NAME}/alpha")
docker_logs=$(docker logs "${container_id}" 2>&1)

# Sanity: the entrypoint banner is in there.
assert_contains "${fleet_logs}" "Container started" "fleet logs missing entrypoint banner"

# Equality with docker logs is the real assertion. Strip trailing newlines
# both sides for a clean compare.
fl_norm=$(printf '%s' "${fleet_logs}" | sed -e 's/[[:space:]]*$//')
dl_norm=$(printf '%s' "${docker_logs}" | sed -e 's/[[:space:]]*$//')
if [ "${fl_norm}" != "${dl_norm}" ]; then
  printf -- '--- fleet logs ---\n%s\n--- docker logs ---\n%s\n--- end ---\n' \
    "${fleet_logs}" "${docker_logs}" >&2
  fail "fleet logs output should match docker logs output"
fi

# ===========================================
# 2. logs against an unknown instance must fail without leaking docker
#    output (the resolver has to error out before invoking docker).
# ===========================================
info "fleet logs ${FIXTURE_REPO_NAME}/no-such (must fail)"
set +e
miss_out=$("${FLEET_BIN}" logs "${FIXTURE_REPO_NAME}/no-such" 2>&1)
miss_rc=$?
set -e
if [ "${miss_rc}" -eq 0 ]; then
  fail "logs on missing instance should fail, got rc=0; out=[${miss_out}]"
fi
assert_contains     "${miss_out}" "no-such"           "missing-instance error should name the instance"
assert_not_contains "${miss_out}" "Container started" "missing-instance error must not leak real container logs"

pass "fleet logs streams container output"
