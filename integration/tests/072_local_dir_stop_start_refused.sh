#!/usr/bin/env bash
# Description: stateless local_dir backend rejects `fleet stop`; `fleet start` no-ops because the instance is always running.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
"${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" --backend local_dir

# `fleet stop` must fail with a clear "does not support" error because
# Stateful() returns false for local_dir.
info "fleet stop alpha (should be refused)"
set +e
stop_out=$("${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/alpha" 2>&1)
rc=$?
set -e
printf '%s\n' "${stop_out}"
if [ "${rc}" -eq 0 ]; then
  fail "fleet stop should fail for local_dir, got rc=0"
fi
assert_contains "${stop_out}" "does not support" "stop error should mention unsupported"

# Status must remain running after a refused stop.
ls_after_stop=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_after_stop}" "running" "status should still be running after refused stop"

# `fleet start` is a no-op because the instance is already at the target
# state ("running"). The lifecycle short-circuits before reaching the
# Stateful check, so this returns success and prints the friendly message.
info "fleet start alpha (already running, should no-op)"
start_out=$("${FLEET_BIN}" start "${FIXTURE_REPO_NAME}/alpha")
printf '%s\n' "${start_out}"
assert_contains "${start_out}" "already running" "start should report already running"

ls_after_start=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_after_start}" "running" "status should still be running after start no-op"

pass "stop refused + start no-op (local_dir)"
