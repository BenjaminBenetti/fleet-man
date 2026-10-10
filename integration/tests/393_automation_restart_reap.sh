#!/usr/bin/env bash
# Description: automation runs survive a daemon restart — a fired trigger's agent
# is launched, fleetd is killed mid-run (SIGKILL, so nothing is flushed on the way
# out), and the restarted daemon reloads ~/.fleet/automation/watch.json and still
# reaps the idle run. Before the watch list was persisted, a restart left every
# running automation instance unreaped for good. Also checks the restarted daemon
# does not fire the trigger a second time.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
# Kill our daemon on exit so its short idle-timeout env (set below) can't bleed
# into a later test's scheduler. Mirrors 391.
itest_cleanup() { pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true; }
itest_begin

# Short idle timeout + tick, as in 391. The daemon inherits this env from the
# client that spawns it, so the restarted daemon (spawned by the second `fleet
# ls` below) gets it too. 60s leaves room to kill and restart the daemon after
# the agent launches but before the first daemon's idle clock would reap it.
export FLEET_AUTOMATION_IDLE_TIMEOUT="60s"
export FLEET_AUTOMATION_TICK_INTERVAL="5s"

setup_automation_test

# wait_no_daemon waits for every `fleet server` of this binary to be gone.
wait_no_daemon() {
  local deadline=$(( $(date +%s) + $(_scale_timeout 10) ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    [ "$(server_count)" = "0" ] && return 0
    sleep 0.2
  done
  return 1
}

info "killing any lingering daemon so we cold-spawn one with our env"
pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
wait_no_daemon || fail "a lingering daemon would not exit"

MARKER="RESTARTREAPMARKER393"

# A cron matching only the current minute, fired by the daemon's startup tick
# (see 391 for why we align to the top of a fresh minute first).
sec="10#$(date +%S)"
if [ "$((sec))" -gt 5 ]; then sleep "$((60 - sec))"; fi
CRON="$(date +'%-M %-H') * * *"
info "seeding automation: agent 'writer' + schedule trigger ('${CRON}')"

cat > "${HOME}/.fleet/state.json" <<EOF
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {
      "name": "${FIXTURE_REPO_NAME}",
      "remote": "${FIXTURE_REPO_URL}",
      "settings": {
        "agents": [
          {
            "name": "writer",
            "command": "claude --system-prompt '\${SYS_PROMPT}' '\${PROMPT}'",
            "systemPrompt": "be-terse",
            "backend": "devcontainer"
          }
        ],
        "triggers": [
          {
            "name": "fire",
            "type": "schedule",
            "agentNames": ["writer"],
            "cron": "${CRON}",
            "prompt": "${MARKER}"
          }
        ]
      },
      "instances": []
    }
  }
}
EOF

info "starting the first daemon + scheduler ('fleet ls')"
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null 2>&1 || true

info "waiting for the scheduler to spawn an instance"
inst=""
for _ in $(seq 1 40); do
  inst=$(grep -oE 'writer-[0-9]+-[0-9]+' "${HOME}/.fleet/state.json" 2>/dev/null | head -1 || true)
  [ -n "${inst}" ] && break
  sleep 3
done
[ -n "${inst}" ] || fail "scheduler never spawned an automation instance"
info "spawned instance: ${inst}"

info "waiting for the agent to launch and receive the prompt"
out=""
for _ in $(seq 1 80); do
  out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/${inst}" -- cat /tmp/automation_agent_out 2>/dev/null || true)
  [ -n "${out}" ] && break
  sleep 3
done
assert_contains "${out}" "AGENT_RAN" "fake claude was never executed (agent did not launch)"
assert_contains "${out}" "${MARKER}" "agent did not receive the trigger prompt"

# The launch is persisted at the end of the tick that launched it.
WATCH="${HOME}/.fleet/automation/watch.json"
info "waiting for the launched run to be persisted to ${WATCH}"
persisted=""
for _ in $(seq 1 20); do
  if grep -qF -- "\"${inst}\"" "${WATCH}" 2>/dev/null && grep -qF '"launched": true' "${WATCH}" 2>/dev/null; then
    persisted=1
    break
  fi
  sleep 1
done
[ -n "${persisted}" ] || fail "launched run never persisted to ${WATCH}"
assert_equals "600" "$(file_mode "${WATCH}")" "watch.json must be private (0600)"

old_pid=$(pgrep -f "${FLEET_BIN} server" | head -1 || true)
[ -n "${old_pid}" ] || fail "no daemon running before the restart"
info "killing the daemon (pid ${old_pid}) with SIGKILL mid-run"
pkill -KILL -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
wait_no_daemon || fail "daemon survived SIGKILL"

grep -qF -- "\"${inst}\"" "${HOME}/.fleet/state.json" \
  || fail "instance was reaped before the restart; the test proves nothing"

info "restarting the daemon ('fleet ls')"
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null 2>&1 || true
new_pid=$(pgrep -f "${FLEET_BIN} server" | head -1 || true)
[ -n "${new_pid}" ] || fail "daemon did not come back"
[ "${new_pid}" != "${old_pid}" ] || fail "daemon pid unchanged (${new_pid}); it was not restarted"

info "waiting for the restarted daemon to reap the idle run"
reaped=""
for _ in $(seq 1 60); do
  if ! grep -qF -- "\"${inst}\"" "${HOME}/.fleet/state.json" 2>/dev/null; then
    reaped=1
    break
  fi
  sleep 3
done
[ -n "${reaped}" ] || fail "the restarted daemon never reaped the automation run"

# fleet.log may have been cut into logs/fleetd/<date>.log by a daemon's startup
# log rotation (it rotates once a day, at the first check past 3am), so search
# both.
cat "${HOME}/.fleet/fleet.log" "${HOME}/.fleet/logs/fleetd/"*.log 2>/dev/null \
  | grep -q "automation: watch list restored" \
  || fail "the restarted daemon did not log restoring the watch list"
# Every firing writes one event log for the trigger (trigger_logs.go).
fires=$(find "${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}/trigger/fire" -name 'event-*.log' 2>/dev/null | wc -l | tr -d ' ')
assert_equals "1" "${fires}" "the trigger fired ${fires} times; a restart must not fire it again"

pass "automation: a run launched before a fleetd restart is still reaped after it"
