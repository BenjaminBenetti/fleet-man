#!/usr/bin/env bash
# itest: k8s
# Description: devcontainer.json host binds stay live: a read-write ~/.aws-style directory syncs both ways (new, changed, nested and deleted files, owned by the host user on the host) and a read-only ~/.gitconfig-style file follows host edits but refuses instance writes, across a stop/start; removing the instance leaves the host files.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
# The daemon reads FLEET_ITEST_HOSTBIND_DIR (below) from its environment; stop
# it so a later test does not inherit it.
itest_cleanup() { pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true; }
itest_begin

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-hostbind" setup_test

# The fixture binds ${localEnv:FLEET_ITEST_HOSTBIND_DIR}/aws and /gitconfig, so
# the bind sources are this test's own files, never the runner's real ones.
export FLEET_ITEST_HOSTBIND_DIR="${TEST_SCRATCH_DIR}/hostbind"
host_aws="${FLEET_ITEST_HOSTBIND_DIR}/aws"
host_gitconfig="${FLEET_ITEST_HOSTBIND_DIR}/gitconfig"
mkdir -p "${host_aws}"
printf '[default]\nregion = itest-1\n' > "${host_aws}/config"
printf '[user]\n\tname = Host Bind\n' > "${host_gitconfig}"

pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
deadline=$(( $(date +%s) + $(_scale_timeout 10) ))
while [ "$(server_count)" != "0" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "a lingering daemon would not exit"
  sleep 0.2
done

inst="${FIXTURE_REPO_NAME}/alpha"
in_instance() { "${FLEET_BIN}" exec "${inst}" -- "$@"; }

# eventually <description> <command...> — poll until the command succeeds. A
# bind is instant; a cluster keeps a copy of each bound path on the fleet's
# share in step with the host (both ways, or host to pod for a read-only one),
# and gets the same check with time to catch up.
eventually() {
  local what="$1" deadline
  shift
  deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "timed out waiting: ${what}"
    sleep 1
  done
}
in_has() { [ "$(in_instance sh -c 'cat "$1" 2>/dev/null' sh "$1")" = "$2" ]; }  # <path> <content>
in_gone() { in_instance sh -c '! test -e "$1"' sh "$1"; }
host_has() { [ "$(cat "$1" 2>/dev/null)" = "$2" ]; }

info "fleet up alpha (host binds)"
fleet_up alpha

check_binds() {  # <when>
  local when="$1"
  info "checking the binds ${when}"
  eventually "the instance sees the host's aws config ${when}" \
    in_has /home/vscode/.aws/config "$(cat "${host_aws}/config")"

  # Host -> instance: a new file, then a change to it.
  printf 'from-host-1 %s\n' "${when}" > "${host_aws}/credentials"
  eventually "a new host file reaches the instance ${when}" \
    in_has /home/vscode/.aws/credentials "from-host-1 ${when}"
  printf 'from-host-2 %s\n' "${when}" > "${host_aws}/credentials"
  eventually "a host edit reaches the instance ${when}" \
    in_has /home/vscode/.aws/credentials "from-host-2 ${when}"

  # Instance -> host: a file in a new nested directory, owned by the host user.
  in_instance sh -c 'mkdir -p "$HOME/.aws/sso/cache" && printf "%s\n" "$1" > "$HOME/.aws/sso/cache/token.json"' sh "token ${when}"
  eventually "an instance write reaches the host ${when}" host_has "${host_aws}/sso/cache/token.json" "token ${when}"
  owner=$(perl -e 'print ((stat shift)[4])' "${host_aws}/sso/cache/token.json")
  assert_equals "$(id -u)" "${owner}" "a file the instance wrote should belong to the host user on the host (${when})"

  # Deletes go both ways too.
  in_instance rm -f /home/vscode/.aws/credentials
  eventually "an instance delete reaches the host ${when}" test ! -e "${host_aws}/credentials"
  rm -rf "${host_aws}/sso"
  eventually "a host delete reaches the instance ${when}" in_gone /home/vscode/.aws/sso

  # The read-only file follows the host (edited in place, as git config does)
  # and refuses writes from the instance.
  printf '[user]\n\tname = Host Bind %s\n' "${when}" > "${host_gitconfig}"
  eventually "a host edit of the read-only file reaches the instance ${when}" \
    in_has /home/vscode/.gitconfig-host "$(cat "${host_gitconfig}")"
  if in_instance sh -c 'printf "x\n" >> "$HOME/.gitconfig-host"' >/dev/null 2>&1; then
    fail "the instance could write the read-only bind (${when})"
  fi
  assert_equals "$(printf '[user]\n\tname = Host Bind %s' "${when}")" "$(cat "${host_gitconfig}")" \
    "a refused write must leave the host file as it was (${when})"
}
check_binds "after create"

info "fleet stop + start alpha"
"${FLEET_BIN}" stop "${inst}" >/dev/null
printf 'while-stopped\n' > "${host_aws}/while-stopped"
"${FLEET_BIN}" start "${inst}" >/dev/null
eventually "a file the host wrote while the instance was stopped is there after start" \
  in_has /home/vscode/.aws/while-stopped "while-stopped"
check_binds "after a stop and start"

info "fleet down alpha"
"${FLEET_BIN}" down "${inst}" >/dev/null
assert_equals "$(printf '[default]\nregion = itest-1')" "$(cat "${host_aws}/config")" \
  "removing the instance must leave the host's bound directory alone"
assert_file_exists "${host_gitconfig}"

pass "host binds: rw dir two-way, ro file host-to-instance, across stop/start"
