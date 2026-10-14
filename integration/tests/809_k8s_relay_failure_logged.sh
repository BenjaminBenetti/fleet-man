#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, a pod agent's in-pod relay failing (root in the pod takes the MCP socket's name with what the agent cannot remove) reaches fleet.log at level=ERROR with its fleet and instance, as the same failure in fleetd would on docker, and the daemon log viewer's Error filter in the TUI shows it.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the pod agent's relays"

setup_test
info "seeding the fleet with Fleet MCP on (its pods bind mcp.sock)"
cat > "${HOME}/.fleet/state.json" <<EOF2
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {
      "name": "${FIXTURE_REPO_NAME}",
      "remote": "${FIXTURE_REPO_URL}",
      "settings": {
        "fleetMcp": true
      },
      "instances": []
    }
  }
}
EOF2
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"
cid=$(itest_container_id "${inst}")
itest_control_test "${inst}" -S mcp.sock || fail "the pod has no mcp.sock"
assert_equals "755" "$(itest_control_mode "${inst}" .)" "the pod's control dir is root's alone"

info "root in the pod takes the socket's name with a directory the agent cannot remove"
itest_container_exec_as "${cid}" root sh -c 'rm -f /fleet-mounts/control/mcp.sock && mkdir -p /fleet-mounts/control/mcp.sock/taken'

info "the agent's failure reaches fleet.log at error, tagged with the instance"
log="${HOME}/.fleet/fleet.log"
failure() { grep ' level=ERROR' "${log}" 2>/dev/null | grep 'pod-agent: ' | grep 'instance=alpha' | tail -n 1 || true; }
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
line=""
until [ -n "${line}" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "no pod-agent error for alpha in fleet.log"
  sleep 0.5
  line=$(failure)
done
printf 'fleet.log: %s\n' "${line}"
assert_contains "${line}" "fleet=${FIXTURE_REPO_NAME}" "the error is not tagged with its fleet"
assert_contains "${line}" "/fleet-mounts/control/mcp.sock" "the error does not name the socket"
assert_contains "${line}" "pod-agent: the control directory is not as fleetd wants it" "the error's message"

info "the daemon log viewer's Error filter shows it"
tui_spawn
tui_wait_for "alpha" 30 || fail "the TUI never listed alpha"
tui_wait_for "settings" 15 || fail "no settings row"
tui_send j
sleep 0.5
tui_send j
sleep 0.5
tui_send Enter
tui_wait_for "Tmux vim keys" 10 || fail "Settings did not open"
found=0
for _ in $(seq 1 120); do
  if tui_capture | grep -q "enter: stream fleet.log"; then
    found=1
    break
  fi
  tui_send j
  sleep 0.2
done
[ "${found}" = 1 ] || fail "never reached the Fleet Daemon Logs row"
tui_send l
sleep 0.3
tui_send Enter
tui_wait_for "fleet.log [Error]" 10 || fail "the Error log stream did not open"
tui_wait_for "pod-agent: the control directory" 20 || fail "the Error filter does not show the pod agent's failure"
tui_send C-c
tui_wait_for "enter: stream fleet.log" 10 || fail "the log stream did not return to Settings"

pass "a pod agent's relay failure shows in fleet.log at error and in the daemon log viewer's Error filter"
