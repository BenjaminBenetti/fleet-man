#!/usr/bin/env bash
# itest: k8s
# Description: VS Code Remote-SSH plumbing: `fleet code --print` with only FLEET_GATEWAY set prints a *.fleet alias, and from a shell without it `ssh -F ~/.ssh/fleet_config <alias>` reaches the instance through `fleet ssh-proxy` and its stdio-only ssh-server (remoteUser, exit codes, pty, sftp, tcp and unix-socket forwards); the local alias works without a gateway; a malformed or unknown alias is refused.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
source "$(dirname "$0")/../editor/editor.sh"
forward_pid=""
itest_cleanup() {
  [ -z "${forward_pid}" ] || kill "${forward_pid}" 2>/dev/null || true
  itest_editor_cleanup
}
itest_begin

setup_test
itest_editor_isolate_ssh
itest_editor_gateway_start
fleet_up alpha
itest_editor_gateway_url
inst="${FIXTURE_REPO_NAME}/alpha"
want_user=$("${FLEET_BIN}" exec "${inst}" -- id -un)
want_home=$("${FLEET_BIN}" exec "${inst}" -- sh -c 'echo "$HOME"')
info "remoteUser ${want_user}, home ${want_home}"

info "fleet code --print with only FLEET_GATEWAY set"
note="${TEST_SCRATCH_DIR}/print.err"
printed=$(env -u FLEET_TOKEN FLEET_GATEWAY="${ITEST_EDITOR_GATEWAY_URL}" "${FLEET_BIN}" code --print "${inst}" 2>"${note}")
info "printed: ${printed}"
assert_equals "1" "$(printf '%s\n' "${printed}" | wc -l | tr -d '[:space:]')" "--print should print one line"
alias=$(itest_editor_uri_part "${printed}" alias)
folder=$(itest_editor_uri_part "${printed}" folder)
info "alias ${alias}, folder ${folder}"
printf '%s' "${alias}" | grep -qE "^alpha\.${FIXTURE_REPO_NAME}\.r[0-9a-f]{6}\.fleet$" \
  || fail "the alias should name the instance, the fleet and the remote: ${alias}"
assert_contains "$(cat "${note}")" "Include fleet_config" "--print should say the Include is missing"
[ ! -e "${HOME}/.ssh/config" ] || fail "--print touched ~/.ssh/config"

cfg="${HOME}/.ssh/fleet_config"
assert_file_exists "${cfg}"
assert_equals "600" "$(file_mode "${cfg}")" "fleet_config mode"
assert_contains "$(cat "${cfg}")" "Host *.fleet" "fleet_config host pattern"
assert_contains "$(cat "${cfg}")" "ssh-proxy %n" "fleet_config proxy command"
token=$(cat "${HOME}/.fleet/mcp.token")
assert_not_contains "$(cat "${cfg}")" "${token}" "fleet_config must hold no token"

# The remote is recorded on the local daemon (here the same daemon): its
# gateway URL, but no token, since FLEET_TOKEN gave none.
records="${HOME}/.fleet/editor_endpoints.json"
assert_file_exists "${records}"
assert_equals "600" "$(file_mode "${records}")" "editor_endpoints.json mode"
assert_contains "$(cat "${records}")" "${ITEST_EDITOR_GATEWAY_URL}" "the recorded gateway"
assert_not_contains "$(cat "${records}")" "${token}" "no token without FLEET_TOKEN"

info "ssh to the alias from a shell without FLEET_GATEWAY"
assert_equals "${want_user}" "$(itest_editor_ssh "${alias}" whoami)" "whoami over Remote-SSH"
assert_equals "${want_home}" "$(itest_editor_ssh "${alias}" pwd)" "a session starts in the home"
assert_equals "yes" "$(itest_editor_ssh "${alias}" "test -f '${folder}/README.md' && echo yes")" "the printed folder holds the checkout"

info "exit codes and output streams"
set +e
itest_editor_ssh "${alias}" 'echo out; echo err >&2; exit 7' >"${TEST_SCRATCH_DIR}/o" 2>"${TEST_SCRATCH_DIR}/e"
rc=$?
set -e
assert_equals "7" "${rc}" "the remote exit code"
assert_equals "out" "$(cat "${TEST_SCRATCH_DIR}/o")" "stdout"
assert_contains "$(cat "${TEST_SCRATCH_DIR}/e")" "err" "stderr"

info "a pty"
tty_out=$(itest_editor_ssh -tt "${alias}" 'tty; echo "TERM=$TERM"' </dev/null | tr -d '\r')
assert_contains "${tty_out}" "/dev/pts/" "a pty session has a terminal"

info "sftp"
printf 'over sftp %s\n' "$(date +%s)" > "${TEST_SCRATCH_DIR}/up.txt"
cat > "${TEST_SCRATCH_DIR}/batch" <<EOF
put ${TEST_SCRATCH_DIR}/up.txt itest-sftp.txt
get itest-sftp.txt ${TEST_SCRATCH_DIR}/down.txt
EOF
itest_editor_sftp -b "${TEST_SCRATCH_DIR}/batch" "${alias}" >/dev/null || fail "sftp failed"
assert_equals "$(cat "${TEST_SCRATCH_DIR}/up.txt")" "$(cat "${TEST_SCRATCH_DIR}/down.txt")" "the file through sftp"
assert_equals "$(cat "${TEST_SCRATCH_DIR}/up.txt")" "$("${FLEET_BIN}" exec "${inst}" -- cat "${want_home}/itest-sftp.txt")" "sftp wrote into the home"

info "forwards reach loopback-only services in the instance"
"${FLEET_BIN}" exec "${inst}" -- sh -c "nohup perl -MIO::Socket::INET -e 'my \$s = IO::Socket::INET->new(LocalAddr => \"127.0.0.1\", LocalPort => 17395, Listen => 8, ReuseAddr => 1) or die \"listen: \$!\"; print STDERR \"listening\\n\"; while (my \$c = \$s->accept) { my \$l = <\$c>; print \$c \"editor-tcp:\$l\"; close \$c }' >/tmp/itest-editor-tcp.log 2>&1 </dev/null &"
"${FLEET_BIN}" exec "${inst}" -- sh -c "nohup perl -MIO::Socket::UNIX -e 'unlink \"/tmp/itest-editor.sock\"; my \$s = IO::Socket::UNIX->new(Local => \"/tmp/itest-editor.sock\", Listen => 8) or die \"listen: \$!\"; print STDERR \"listening\\n\"; while (my \$c = \$s->accept) { my \$l = <\$c>; print \$c \"editor-unix:\$l\"; close \$c }' >/tmp/itest-editor-unix.log 2>&1 </dev/null &"
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
until "${FLEET_BIN}" exec "${inst}" -- sh -c 'grep -q listening /tmp/itest-editor-tcp.log && grep -q listening /tmp/itest-editor-unix.log' 2>/dev/null; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the in-instance servers never listened"
  sleep 0.5
done
assert_equals "editor-tcp:ping" "$(printf 'ping\n' | itest_editor_ssh -W 127.0.0.1:17395 "${alias}")" "direct-tcpip (ssh -W)"
local_sock="${TEST_SCRATCH_DIR}/fwd.sock"
itest_editor_ssh -N -L "${local_sock}:/tmp/itest-editor.sock" "${alias}" &
forward_pid=$!
deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
until [ -S "${local_sock}" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "ssh -L never made ${local_sock}"
  kill -0 "${forward_pid}" 2>/dev/null || fail "ssh -L exited"
  sleep 0.2
done
got=$(python3 -c 'import socket, sys; s = socket.socket(socket.AF_UNIX); s.connect(sys.argv[1]); s.sendall(b"ping\n"); print(s.makefile().readline().strip())' "${local_sock}")
assert_equals "editor-unix:ping" "${got}" "direct-streamlocal (ssh -L to a unix socket)"
kill "${forward_pid}" 2>/dev/null || true
wait "${forward_pid}" 2>/dev/null || true
forward_pid=""

info "the local alias reaches the instance through the local daemon"
assert_equals "${want_user}" "$(itest_editor_ssh "alpha.${FIXTURE_REPO_NAME}.fleet" whoami)" "whoami over the local alias"

info "malformed and unknown aliases are refused"
set +e
bad=$(env -u FLEET_GATEWAY "${FLEET_BIN}" ssh-proxy 'alpha;id.x.fleet' </dev/null 2>&1)
rc=$?
set -e
[ "${rc}" -ne 0 ] || fail "ssh-proxy took a malformed alias"
assert_contains "${bad}" "is not a fleet ssh alias" "the refusal names the grammar"
set +e
unknown=$(itest_editor_ssh "alpha.${FIXTURE_REPO_NAME}.r000000.fleet" true 2>&1)
rc=$?
set -e
[ "${rc}" -ne 0 ] || fail "an alias for an unknown remote connected"
assert_contains "${unknown}" "no remote fleet daemon is known" "the unknown remote is named"

pass "Remote-SSH through fleet ssh-proxy and the stdio ssh-server"
