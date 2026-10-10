#!/usr/bin/env bash
# Description: `fleet logs` streams the container's own log output, including the entrypoint banner.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

container_id=$(itest_container_id "${FIXTURE_REPO_NAME}/alpha")
[ -n "${container_id}" ] || fail "could not read container_id from state.json"

# ===========================================
# 1. `fleet logs` (non-follow) returns the container's stdout. The
#    fixture's entrypoint runs `echo Container started`, so that
#    sentinel should appear. We compare against the backend's own log of
#    the container (docker logs) to make sure the two outputs agree —
#    that's the contract.
# ===========================================
info "fleet logs alpha (non-follow)"
fleet_logs=$("${FLEET_BIN}" logs "${FIXTURE_REPO_NAME}/alpha")
container_logs=$(itest_container_logs "${container_id}")

# Sanity: the entrypoint banner is in there.
assert_contains "${fleet_logs}" "Container started" "fleet logs missing entrypoint banner"

# Equality with the container log is the real assertion. Strip trailing newlines
# both sides for a clean compare.
fl_norm=$(printf '%s' "${fleet_logs}" | sed -e 's/[[:space:]]*$//')
cl_norm=$(printf '%s' "${container_logs}" | sed -e 's/[[:space:]]*$//')
if [ "${fl_norm}" != "${cl_norm}" ]; then
  printf -- '--- fleet logs ---\n%s\n--- container logs ---\n%s\n--- end ---\n' \
    "${fleet_logs}" "${container_logs}" >&2
  fail "fleet logs output should match the container log"
fi

# ===========================================
# 2. logs against an unknown instance must fail without leaking container
#    output (the resolver has to error out before asking the backend).
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
