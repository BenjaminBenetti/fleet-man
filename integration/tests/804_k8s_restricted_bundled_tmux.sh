#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, in a namespace that enforces the restricted pod security standard (no root), an image without tmux gets tmux sessions through fleet's bundled static tmux: create, sessions made from the CLI and from a TUI attach, and the session list all work, and nothing runs apt.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
NS=""
itest_cleanup() {
  tui_kill
  if [ -n "${NS}" ]; then
    _be_kubectl delete namespace "${NS}" --wait=false >/dev/null 2>&1 || true
  fi
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the bundled tmux on a restricted namespace"

tmux_var="FLEET_POD_TMUX_$(_be_arch | tr '[:lower:]' '[:upper:]')"
[ -f "${!tmux_var:-}" ] || fail "${tmux_var} names no tmux bundle (make pod-tmux): a tmux-less image needs it"

setup_test

# A namespace of its own, enforcing restricted, as a second connection.
NS="${FLEET_ITEST_NAMESPACE}-restricted"
_be_kubectl create namespace "${NS}" >/dev/null 2>&1 || true
_be_kubectl label --overwrite namespace "${NS}" pod-security.kubernetes.io/enforce=restricted >/dev/null
out=$("${FLEET_BIN}" cluster add "${FLEET_ITEST_CLUSTER}" --name restricted -n "${NS}" 2>&1) || fail "cluster add: ${out}"
test_out=$("${FLEET_BIN}" cluster test restricted 2>&1) || true
assert_contains "$(printf '%s\n' "${test_out}" | tail -n 1)" "limited" "a restricted namespace tests limited"

# The image has no tmux, and no hook installs one.
cat >"${FIXTURE_REPO_DIR}/.devcontainer/devcontainer.json" <<'JSON'
{
  "name": "fleet-man restricted fixture",
  "image": "mcr.microsoft.com/devcontainers/base:debian-12",
  "remoteUser": "vscode",
  "postStartCommand": "echo poststart >> $HOME/.fleet-poststart.log"
}
JSON
(cd "${FIXTURE_REPO_DIR}" && git commit -qam "fixture: no tmux")

inst="${FIXTURE_REPO_NAME}/alpha"
info "fleet up alpha on the restricted connection"
"${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" --cluster restricted

uid=$("${FLEET_BIN}" exec "${inst}" -- id -u)
[ "${uid}" != "0" ] || fail "the restricted pod runs as root"
in_image=$("${FLEET_BIN}" exec "${inst}" -- sh -c 'ls /usr/bin/tmux /usr/local/bin/tmux 2>/dev/null || true')
assert_equals "" "${in_image}" "the image itself has no tmux"

info "a session from a TUI attach, through the bundled tmux"
tui_spawn
tui_wait_for "alpha" 30 || fail "the TUI never listed alpha"
tui_wait_for "○ idle" 90 || fail "alpha never showed running"
tui_cursor_to "▶ ○ alpha"
tui_send Enter
sessions=""
for _ in $(seq 1 60); do
  sessions=$("${FLEET_BIN}" exec "${inst}" -- sh -c 'tmux list-sessions -F "#{session_name}"' 2>/dev/null || true)
  if printf '%s\n' "${sessions}" | grep -qE '^alpha(~|$)'; then break; fi
  sleep 1
done
# The pane attaches a session of the instance (alpha, or a group alpha~<id>).
printf '%s\n' "${sessions}" | grep -qE '^alpha(~|$)' || fail "the attach left no alpha session: ${sessions}"
which_tmux=$("${FLEET_BIN}" exec "${inst}" -- sh -c 'command -v tmux')
assert_contains "${which_tmux}" "/opt/fleet/tmux" "the sessions' tmux is the bundled one"

info "a session from the CLI, and the session list"
out=$("${FLEET_BIN}" spawn-session "${inst}" work 2>&1) || fail "spawn-session: ${out}"
"${FLEET_BIN}" exec-in-session "${inst}" work 'echo TMUX_$((6*7))' >/dev/null
found=""
for _ in $(seq 1 40); do
  if "${FLEET_BIN}" read-session "${inst}" work 2>/dev/null | grep -qF "TMUX_42"; then found=1; break; fi
  sleep 0.5
done
[ -n "${found}" ] || fail "the CLI session never ran its command"
listed=$(itest_mcp_call fleet_session_list "{\"fleet\":\"${FIXTURE_REPO_NAME}\",\"instance\":\"alpha\"}")
assert_contains "${listed}" "alpha~work" "fleet_session_list lists the CLI session"

info "nothing ran apt"
apt_log=$("${FLEET_BIN}" exec "${inst}" -- sh -c 'cat /var/log/apt/history.log 2>/dev/null || true')
assert_not_contains "${apt_log}" "tmux" "tmux was installed with apt"

pass "a tmux-less image on a restricted namespace gets sessions through the bundled tmux, no apt"
