#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, a running instance watched by a TUI (its sessions, screens, agent, stats and branch polled every second or so) costs the API server no exec upgrade in steady state: the pollers ride the pod's one agent session.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: exec upgrades on the API server"

setup_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"
"${FLEET_BIN}" spawn-session "${inst}" work >/dev/null

# exec_upgrades — the API server's count of pod exec requests (CONNECT on the
# exec subresource), from its metrics.
exec_upgrades() {
  kubectl --context "${FLEET_ITEST_CLUSTER}" get --raw /metrics 2>/dev/null \
    | awk '/^apiserver_request_total\{/ && /verb="CONNECT"/ && /subresource="exec"/ { n += $NF } END { printf "%d\n", n }'
}

tui_spawn
tui_wait_for "alpha" 30 || fail "the TUI never listed alpha"
tui_wait_for "○ idle" 90 || fail "alpha never showed running"
tui_cursor_to "▶ ○ alpha"
tui_send Space
tui_wait_for "○ work" 20 || fail "the session list never showed"
# Let the agent session and the pollers settle.
sleep 10

before=$(exec_upgrades)
# The create's seed feed and the agent's bootstrap were exec upgrades: a
# metric that counts none is not the one this test reads.
[ "${before}" -gt 0 ] || fail "the API server's metrics count no pod exec at all"
info "watching for 60s (exec upgrades so far: ${before})"
sleep 60
after=$(exec_upgrades)
info "exec upgrades after: ${after}"
tui_assert_contains "○ work" "the session list should still be polled"
assert_equals "0" "$(( after - before ))" "exec upgrades during 60s of steady-state polling"

pass "0 exec upgrades in a minute of TUI polling"
