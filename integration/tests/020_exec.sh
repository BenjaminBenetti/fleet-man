#!/usr/bin/env bash
# itest: k8s
# Description: `fleet exec` runs commands inside the container and propagates exit codes.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

info "fleet exec alpha -- uname -s"
uname_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- uname -s)
assert_equals "Linux" "${uname_out}" "uname -s inside container"

info "fleet exec alpha -- sh -c 'echo hello-from-container'"
hello_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "echo hello-from-container")
assert_equals "hello-from-container" "${hello_out}" "echo output"

# A non-zero exit inside the container should propagate.
info "fleet exec alpha -- sh -c 'exit 7' should fail"
set +e
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "exit 7" >/dev/null 2>&1
rc=$?
set -e
if [ "${rc}" -eq 0 ]; then
  fail "expected non-zero exit from 'exit 7', got 0"
fi

pass "exec"
