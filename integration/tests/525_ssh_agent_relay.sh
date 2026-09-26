#!/usr/bin/env bash
# Description: instances reach the daemon's SSH agent through the per-instance relay socket — including a container user with another uid — and keep it across a daemon restart.
set -euo pipefail

source "$(dirname "$0")/../common.sh"

agent_dir=""
test_agent_pid=""
itest_cleanup() {
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  # Only the agent this test started — never one inherited from the runner.
  if [ -n "${test_agent_pid}" ]; then kill "${test_agent_pid}" >/dev/null 2>&1 || true; fi
  if [ -n "${agent_dir}" ]; then rm -rf "${agent_dir}"; fi
}
itest_begin

# The two-user fixture adds a second container user (`app`, uid 4001) next to
# the remapped remoteUser: the relay must serve it too.
setup_twouser_test

# A throwaway agent holding a fresh key. The daemon relays the agent it was
# started with, so it must be (re)started from this environment. A short dir:
# unix socket paths are capped at 108 bytes.
agent_dir="$(mktemp -d /tmp/fleet-agent.XXXXXX)"
ssh-keygen -q -t ed25519 -N '' -C fleet-itest-key -f "${agent_dir}/key"
eval "$(ssh-agent -a "${agent_dir}/agent.sock" -s)" >/dev/null
test_agent_pid="${SSH_AGENT_PID}"
ssh-add -q "${agent_dir}/key"
fingerprint="$(ssh-keygen -lf "${agent_dir}/key.pub" | awk '{print $2}')"
info "test key ${fingerprint}"

stop_daemon() {
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  local deadline=$(( $(date +%s) + $(_scale_timeout 10) ))
  while [ "$(server_count)" != "0" ] && [ "$(date +%s)" -lt "${deadline}" ]; do
    sleep 0.2
  done
  assert_equals "0" "$(server_count)" "daemon should be stopped"
}
stop_daemon

info "fleet up alpha"
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

host_sock="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/alpha/.control/ssh-agent.sock"
info "asserting the relay socket exists on the host: ${host_sock}"
[ -S "${host_sock}" ] || fail "no relay socket at ${host_sock}"
assert_equals "666" "$(file_mode "${host_sock}")" "instance relay socket mode (served per peer check)"

info "asserting the instance's SSH_AUTH_SOCK is the relay and lists the key"
out="$("${FLEET_BIN}" exec "${inst}" -- sh -c 'echo "sock=${SSH_AUTH_SOCK}"; ssh-add -l')"
assert_contains "${out}" "sock=/fleet-mounts/control/ssh-agent.sock" "SSH_AUTH_SOCK inside the instance"
assert_contains "${out}" "${fingerprint}" "the host agent's key through the relay"

info "asserting signing works end to end"
out="$("${FLEET_BIN}" exec "${inst}" -- sh -c 'cd /tmp && echo msg > m && ssh-add -L > k.pub && ssh-keygen -q -Y sign -f k.pub -n fleet-itest m && test -s m.sig && echo SIGNED')"
assert_contains "${out}" "SIGNED" "ssh-keygen -Y sign through the relay"

info "asserting a container user with another uid (app, 4001) is served"
# runuser, not sudo -u: the base image lets vscode sudo only to root.
out="$("${FLEET_BIN}" exec "${inst}" -- sudo runuser -u app -- env SSH_AUTH_SOCK=/fleet-mounts/control/ssh-agent.sock ssh-add -l)"
assert_contains "${out}" "${fingerprint}" "cross-uid container user through the relay"

info "asserting the instance cannot manage the agent (ssh-add -D is refused)"
set +e
out="$("${FLEET_BIN}" exec "${inst}" -- ssh-add -D 2>&1)"
set -e
assert_contains "${out}" "Failed" "ssh-add -D through the relay"
ssh-add -l | grep -q "${fingerprint}" || fail "the host agent lost its key through the relay"

info "restarting the daemon: the running instance keeps its agent (no pinned socket inode)"
stop_daemon
"${FLEET_BIN}" ls >/dev/null
# Wait for the NEW daemon to serve the instance socket: a leftover file from
# the old one would pass a mere existence check.
out=""
deadline=$(( $(date +%s) + $(_scale_timeout 20) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  out="$("${FLEET_BIN}" exec "${inst}" -- ssh-add -l 2>&1 || true)"
  case "${out}" in *"${fingerprint}"*) break ;; esac
  sleep 0.5
done
assert_contains "${out}" "${fingerprint}" "the key after a daemon restart"

pass "instances use the host agent through the relay"
