#!/usr/bin/env bash
# Description: `fleet copy` is bidirectional scp-style — out of, into, and between instances, preserving content and mode.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

workdir=$(mktemp -d)
trap 'rm -rf "${workdir}"' EXIT

info "create a file inside the instance"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "printf 'copied-bytes' > /tmp/fc-test.bin && chmod 755 /tmp/fc-test.bin"

info "fleet copy alpha:/tmp/fc-test.bin (absolute path, explicit dest)"
"${FLEET_BIN}" copy "${FIXTURE_REPO_NAME}/alpha:/tmp/fc-test.bin" "${workdir}/out.bin"
assert_equals "copied-bytes" "$(cat "${workdir}/out.bin")" "copied file content"
mode=$(stat -c '%a' "${workdir}/out.bin")
assert_equals "755" "${mode}" "copied file keeps its mode"

info "fleet copy into a directory keeps the source basename"
"${FLEET_BIN}" copy "${FIXTURE_REPO_NAME}/alpha:/tmp/fc-test.bin" "${workdir}/"
assert_equals "copied-bytes" "$(cat "${workdir}/fc-test.bin")" "directory dest content"

info "fleet copy of a relative path resolves against the workspace folder"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "printf 'relative-bytes' > ws-rel.txt"
"${FLEET_BIN}" copy "${FIXTURE_REPO_NAME}/alpha:ws-rel.txt" "${workdir}/rel.txt"
assert_equals "relative-bytes" "$(cat "${workdir}/rel.txt")" "relative source content"

info "fleet copy of a missing file fails"
set +e
"${FLEET_BIN}" copy "${FIXTURE_REPO_NAME}/alpha:/tmp/does-not-exist" "${workdir}/nope" >/dev/null 2>&1
rc=$?
set -e
if [ "${rc}" -eq 0 ]; then
  fail "expected non-zero exit copying a missing file, got 0"
fi
if [ -e "${workdir}/nope" ]; then
  fail "failed copy must not leave a destination file"
fi

info "fleet copy of a directory fails"
set +e
"${FLEET_BIN}" copy "${FIXTURE_REPO_NAME}/alpha:/tmp" "${workdir}/dir-nope" >/dev/null 2>&1
rc=$?
set -e
if [ "${rc}" -eq 0 ]; then
  fail "expected non-zero exit copying a directory, got 0"
fi

# ---- the reverse direction: copy a local file INTO an instance ----

info "fleet copy <local> alpha:/tmp/up.bin uploads into the instance, keeping mode"
printf 'uploaded-bytes' > "${workdir}/up.bin"
chmod 755 "${workdir}/up.bin"
"${FLEET_BIN}" copy "${workdir}/up.bin" "${FIXTURE_REPO_NAME}/alpha:/tmp/up.bin"
assert_equals "uploaded-bytes" "$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /tmp/up.bin)" "uploaded file content"
assert_equals "755" "$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- stat -c '%a' /tmp/up.bin)" "uploaded file keeps its mode"

info "fleet copy into an instance directory keeps the source basename"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- mkdir -p /tmp/updir
"${FLEET_BIN}" copy "${workdir}/up.bin" "${FIXTURE_REPO_NAME}/alpha:/tmp/updir/"
assert_equals "uploaded-bytes" "$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /tmp/updir/up.bin)" "directory upload basename"

info "fleet copy into a non-existent instance directory fails"
set +e
"${FLEET_BIN}" copy "${workdir}/up.bin" "${FIXTURE_REPO_NAME}/alpha:/tmp/no-such-dir/" >/dev/null 2>&1
rc=$?
set -e
if [ "${rc}" -eq 0 ]; then
  fail "expected non-zero exit uploading into a missing directory, got 0"
fi

info "fleet copy of a missing local file fails"
set +e
"${FLEET_BIN}" copy "${workdir}/ghost-local" "${FIXTURE_REPO_NAME}/alpha:/tmp/ghost" >/dev/null 2>&1
rc=$?
set -e
if [ "${rc}" -eq 0 ]; then
  fail "expected non-zero exit uploading a missing local file, got 0"
fi

# ---- instance → instance (the relay path) ----

info "fleet copy alpha:path alpha:path2 relays between two instance paths"
"${FLEET_BIN}" copy "${FIXTURE_REPO_NAME}/alpha:/tmp/up.bin" "${FIXTURE_REPO_NAME}/alpha:/tmp/relayed.bin"
assert_equals "uploaded-bytes" "$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /tmp/relayed.bin)" "relayed file content"

pass "copy"
