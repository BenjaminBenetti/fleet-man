#!/usr/bin/env bash
# itest: k8s
# Description: fleet_copy over MCP — the daemon copies a file or a directory out of, into, and between instances (a non-instance path is on the daemon host, ~/ its home), byte-exact across many data frames; a relative host path, a half-named instance, a stopped instance and a missing source are tool errors.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"
alpha="\"fleet\":\"${FIXTURE_REPO_NAME}\",\"instance\":\"alpha\""

workdir=$(mktemp -d)
trap 'rm -rf "${workdir}"' EXIT

# copy <src-json> <dst-json> [<field>] — fleet_copy {src, dst}.
copy() {
  itest_mcp_call fleet_copy "{\"src\":$1,\"dst\":$2}" "${3:-}"
}

# ===========================================
# Out of, into, and between instances.
# ===========================================
info "fleet_copy uploads a multi-frame host file into the instance"
head -c 1048577 /dev/urandom > "${workdir}/big.bin"
chmod 750 "${workdir}/big.bin"
want_sum=$(sha256sum < "${workdir}/big.bin" | cut -d' ' -f1)
"${FLEET_BIN}" exec "${inst}" -- mkdir -p /tmp/mcpcopy
res=$(copy "{\"path\":\"${workdir}/big.bin\"}" "{${alpha},\"path\":\"/tmp/mcpcopy/\"}") \
  || fail "fleet_copy (upload) did not answer"
assert_equals "/tmp/mcpcopy/big.bin" "$(printf '%s' "${res}" | itest_json_field structuredContent.path)" "upload lands inside the directory, keeping the source name"
assert_equals "1048577" "$(printf '%s' "${res}" | itest_json_field structuredContent.bytes)" "upload byte count"
got_sum=$("${FLEET_BIN}" exec "${inst}" -- sh -c 'sha256sum < /tmp/mcpcopy/big.bin' | cut -d' ' -f1)
assert_equals "${want_sum}" "${got_sum}" "the uploaded file is byte-identical"

info "fleet_copy relays between two instance paths"
copy "{${alpha},\"path\":\"/tmp/mcpcopy/big.bin\"}" "{${alpha},\"path\":\"/tmp/mcpcopy/relayed.bin\"}" >/dev/null \
  || fail "fleet_copy (relay) did not answer"
got_sum=$("${FLEET_BIN}" exec "${inst}" -- sh -c 'sha256sum < /tmp/mcpcopy/relayed.bin' | cut -d' ' -f1)
assert_equals "${want_sum}" "${got_sum}" "the relayed file is byte-identical"

info "fleet_copy downloads to the daemon host, keeping the mode"
out=$(copy "{${alpha},\"path\":\"/tmp/mcpcopy/relayed.bin\"}" "{\"path\":\"${workdir}/down.bin\"}" structuredContent.path)
assert_equals "${workdir}/down.bin" "${out}" "download reports the host path it wrote"
assert_equals "${want_sum}" "$(sha256sum < "${workdir}/down.bin" | cut -d' ' -f1)" "the downloaded file is byte-identical"
assert_equals "750" "$(file_mode "${workdir}/down.bin")" "the downloaded file keeps its mode"

info "a relative instance path is the workspace folder; ~/ is the daemon user's home"
"${FLEET_BIN}" exec "${inst}" -- sh -c "printf 'ws-bytes' > mcp-ws.txt"
mkdir -p "${HOME}/mcpcopy-home"
out=$(copy "{${alpha},\"path\":\"mcp-ws.txt\"}" '{"path":"~/mcpcopy-home/"}' structuredContent.path)
assert_equals "${HOME}/mcpcopy-home/mcp-ws.txt" "${out}" "~/ resolves against the daemon user's home"
assert_equals "ws-bytes" "$(cat "${HOME}/mcpcopy-home/mcp-ws.txt")" "the workspace-relative source was copied"

info "fleet_copy copies a directory tree in and back out"
mkdir -p "${workdir}/tree/sub"
printf 'top' > "${workdir}/tree/top.txt"
printf 'nested' > "${workdir}/tree/sub/nested.txt"
copy "{\"path\":\"${workdir}/tree\"}" "{${alpha},\"path\":\"/tmp/mcptree\"}" >/dev/null \
  || fail "fleet_copy (directory upload) did not answer"
assert_equals "nested" "$("${FLEET_BIN}" exec "${inst}" -- cat /tmp/mcptree/sub/nested.txt)" "the uploaded tree's nested file"
copy "{${alpha},\"path\":\"/tmp/mcptree\"}" "{\"path\":\"${workdir}/back\"}" >/dev/null \
  || fail "fleet_copy (directory download) did not answer"
assert_equals "top" "$(cat "${workdir}/back/top.txt")" "the downloaded tree's top file"
assert_equals "nested" "$(cat "${workdir}/back/sub/nested.txt")" "the downloaded tree's nested file"

# ===========================================
# Errors are tool errors with a readable message.
# ===========================================
# expect_copy_error <src-json> <dst-json> <needle> <what>
expect_copy_error() {
  local res
  res=$(copy "$1" "$2") || fail "fleet_copy did not answer ($4)"
  assert_equals "true" "$(printf '%s' "${res}" | itest_json_field isError)" "$4 should be a tool error"
  assert_contains "$(printf '%s' "${res}" | itest_json_field content.0.text)" "$3" "$4 should say why"
}

info "fleet_copy refuses bad endpoints"
expect_copy_error "{${alpha},\"path\":\"mcp-ws.txt\"}" '{"path":"relative.txt"}' "must be absolute" "a relative host path"
expect_copy_error "{\"instance\":\"alpha\",\"path\":\"mcp-ws.txt\"}" "{\"path\":\"${workdir}/x\"}" "both fleet and instance" "an instance without its fleet"
expect_copy_error "{${alpha},\"path\":\"/tmp/does-not-exist\"}" "{\"path\":\"${workdir}/nope\"}" "does-not-exist" "a missing source"
assert_file_absent "${workdir}/nope"

"${FLEET_BIN}" stop "${inst}" >/dev/null
expect_copy_error "{${alpha},\"path\":\"mcp-ws.txt\"}" "{\"path\":\"${workdir}/x\"}" "is not running" "a stopped instance"

pass "fleet_copy uploads, downloads and relays files and trees over MCP"
