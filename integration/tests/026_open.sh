#!/usr/bin/env bash
# Description: `fleet open` copies a file out of an instance and hands it to the opener (FLEET_OPENER); own-machine sources and instance destinations are rejected.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
workdir=$(mktemp -d)
itest_cleanup() { rm -rf "${workdir}"; }
itest_begin

setup_test
fleet_up alpha

# A stub opener standing in for xdg-open: it records every invocation's
# arguments, one line each, so the test can see exactly what was opened.
opener="${workdir}/opener.sh"
log="${workdir}/opened.log"
cat > "${opener}" <<STUB
#!/bin/sh
printf '%s\n' "\$*" >> "${log}"
STUB
chmod +x "${opener}"
export FLEET_OPENER="${opener}"

info "create a file inside the instance"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "printf 'opened-bytes' > /tmp/fo-test.txt"

info "fleet open alpha:/tmp/fo-test.txt (1-arg: copy to the cwd, then open it)"
out=$(cd "${workdir}" && "${FLEET_BIN}" open "${FIXTURE_REPO_NAME}/alpha:/tmp/fo-test.txt")
info "output: ${out}"
assert_contains "${out}" "Opened ${workdir}/fo-test.txt" "open should report the absolute opened path"
assert_equals "opened-bytes" "$(cat "${workdir}/fo-test.txt")" "copied file content"
assert_equals "${workdir}/fo-test.txt" "$(cat "${log}")" "opener should receive the absolute path of the copied file"

info "fleet open into a directory keeps the source basename and opens it there"
mkdir -p "${workdir}/sub"
: > "${log}"
"${FLEET_BIN}" open "${FIXTURE_REPO_NAME}/alpha:/tmp/fo-test.txt" "${workdir}/sub/"
assert_equals "opened-bytes" "$(cat "${workdir}/sub/fo-test.txt")" "directory dest content"
assert_equals "${workdir}/sub/fo-test.txt" "$(cat "${log}")" "opener should receive the directory-dest path"

info "FLEET_OPENER may carry its own arguments; the path is appended last"
: > "${log}"
FLEET_OPENER="${opener} --fullscreen" "${FLEET_BIN}" open "${FIXTURE_REPO_NAME}/alpha:/tmp/fo-test.txt" "${workdir}/sub/"
assert_equals "--fullscreen ${workdir}/sub/fo-test.txt" "$(cat "${log}")" "opener arguments should precede the path"

info "an opener that fails is reported (the copy itself still lands)"
failing="${workdir}/failing.sh"
printf '#!/bin/sh\necho "no handler for this file" >&2\nexit 3\n' > "${failing}"
chmod +x "${failing}"
rm -f "${workdir}/sub/fo-test.txt"
set +e
out=$(FLEET_OPENER="${failing}" "${FLEET_BIN}" open "${FIXTURE_REPO_NAME}/alpha:/tmp/fo-test.txt" "${workdir}/sub/" 2>&1)
rc=$?
set -e
info "output: ${out}"
[ "${rc}" -ne 0 ] || fail "expected non-zero exit when the opener fails, got 0"
assert_contains "${out}" "no handler for this file" "opener failure should surface its stderr"
assert_equals "opened-bytes" "$(cat "${workdir}/sub/fo-test.txt")" "a failed open must not undo the copy"

info "a source already on this machine is rejected (nothing to fetch)"
: > "${log}"
set +e
out=$("${FLEET_BIN}" open "${workdir}/fo-test.txt" 2>&1)
rc=$?
set -e
[ "${rc}" -ne 0 ] || fail "expected non-zero exit opening an own-machine source, got 0"
assert_contains "${out}" "source must be in an instance" "own-machine source error text"
[ ! -s "${log}" ] || fail "a rejected open must not invoke the opener"

info "an instance destination is rejected (the file can only be opened here)"
set +e
out=$("${FLEET_BIN}" open "${FIXTURE_REPO_NAME}/alpha:/tmp/fo-test.txt" "${FIXTURE_REPO_NAME}/alpha:/tmp/elsewhere" 2>&1)
rc=$?
set -e
[ "${rc}" -ne 0 ] || fail "expected non-zero exit for an instance destination, got 0"
assert_contains "${out}" "destination must be on your machine" "instance destination error text"
[ ! -s "${log}" ] || fail "a rejected open must not invoke the opener"

info "fleet open of a missing file fails without opening anything"
set +e
"${FLEET_BIN}" open "${FIXTURE_REPO_NAME}/alpha:/tmp/does-not-exist" "${workdir}/nope" >/dev/null 2>&1
rc=$?
set -e
[ "${rc}" -ne 0 ] || fail "expected non-zero exit opening a missing file, got 0"
[ ! -e "${workdir}/nope" ] || fail "failed open must not leave a destination file"
[ ! -s "${log}" ] || fail "a failed copy must not invoke the opener"

pass "open"
