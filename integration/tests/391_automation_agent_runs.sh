#!/usr/bin/env bash
# Description: end-to-end automation (issue #188) — a fired Schedule trigger
# spawns an instance, launches the agent (which RUNS with the trigger's prompt),
# and then the idle instance is reaped. The fixture installs a fake `claude`
# under ~/.local/bin (on PATH only via .bashrc, like the real install) that
# records its args to /tmp/automation_agent_out then sleeps; we assert the prompt
# reaches it AND that the idle agent's instance is torn down. This is the real
# proof of the whole scheduler -> spawn -> launch -> reap lifecycle.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
# Kill our daemon on exit so its short idle-timeout env (set below) can't bleed
# into a later test's scheduler. Mirrors 601/611.
itest_cleanup() { pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true; }
itest_begin

server_count() { pgrep -fc "${FLEET_BIN} server" 2>/dev/null || true; }

# Shorten the scheduler's idle timeout + tick so the reap half of the lifecycle
# verifies in ~a minute instead of the production 2m/30s. The daemon inherits
# this env (spawn.go: cmd.Env = os.Environ()), so it must be exported BEFORE the
# `fleet ls` below starts the daemon + scheduler. The idle clock only starts once
# the agent launches (post-provision), so 60s leaves a wide margin to read the
# agent's output before the instance is reaped, even on a slow runner.
export FLEET_AUTOMATION_IDLE_TIMEOUT="60s"
export FLEET_AUTOMATION_TICK_INTERVAL="5s"

setup_automation_test

# A daemon lingers across tests (teardown wipes ~/.fleet but never kills the
# Setsid daemon). A prior test's daemon still has the PRODUCTION 2m idle timeout
# and its own scheduler — left alive it would race ours (firing the trigger,
# reaping on the wrong clock). Kill it so the `fleet ls` below cold-spawns
# exactly one fresh daemon that inherits our short-timeout env.
info "killing any lingering daemon so we cold-spawn one with our env"
pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
deadline=$(( $(date +%s) + $(_scale_timeout 5) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  [ "$(server_count)" = "0" ] && break
  sleep 0.2
done

MARKER="AUTOMATIONPROMPTMARKER42"

# A cron matching the CURRENT minute fires exactly once (its only match is this
# minute of this hour; next is tomorrow), so the daemon's immediate startup tick
# fires it without spawning a new instance every minute. The ONLY thing that fires
# it is that startup tick evaluating the cron against the wall clock, so the daemon
# must boot while we're still inside the seeded minute. Align to the top of a fresh
# minute first (unless we're already near it) so there's ~a full minute of margin —
# far more than the sub-second seed + daemon cold-spawn + first tick consumes, even
# on a loaded runner. (Plain wall-clock alignment, not a scaled timeout budget.)
# 10# forces base-10: date +%S is zero-padded ("08"/"09"), which arithmetic
# context would otherwise read as invalid octal and abort under `set -e`.
sec="10#$(date +%S)"
if [ "$((sec))" -gt 5 ]; then sleep "$((60 - sec))"; fi
CRON="$(date +'%-M %-H') * * *"
info "seeding automation: agent 'writer' + schedule trigger ('${CRON}') prompt='${MARKER}'"

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

# Start the daemon (and its scheduler). The immediate startup tick fires the
# due trigger, which creates the instance.
info "starting the daemon + scheduler ('fleet ls')"
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null 2>&1 || true

# Wait for the scheduler to spawn the automation instance (name: writer-<HHMMSS>-<n>).
info "waiting for the scheduler to spawn an instance"
inst=""
for _ in $(seq 1 40); do
  inst=$(grep -oE 'writer-[0-9]+-[0-9]+' "${HOME}/.fleet/state.json" 2>/dev/null | head -1 || true)
  [ -n "${inst}" ] && break
  sleep 3
done
[ -n "${inst}" ] || fail "scheduler never spawned an automation instance"
info "spawned instance: ${inst}"

# Wait for the instance to provision + the agent to launch + write the marker.
# This implicitly waits for the instance to reach 'running' ('fleet exec' fails
# until then) and for launchAutomationCommand to have run the agent.
info "waiting for the agent to launch and receive the prompt"
out=""
for _ in $(seq 1 80); do
  out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/${inst}" -- cat /tmp/automation_agent_out 2>/dev/null || true)
  [ -n "${out}" ] && break
  sleep 3
done

printf 'agent output: %s\n' "${out}"
assert_contains "${out}" "AGENT_RAN"  "fake claude was never executed (agent did not launch)"
assert_contains "${out}" "${MARKER}"  "agent did not receive the trigger prompt"
assert_contains "${out}" "be-terse"   "agent did not receive the system prompt"
info "agent ran with the prompt; now waiting for the idle reaper to tear it down"

# The fake claude `exec sleep 600`s, so the agent stays alive but produces no
# screen output → the activity detector reports it idle (the Claude hook detector
# sees no state file → StateWaiting; the screen-diff fallback sees a static pane →
# StateWaiting), so after FLEET_AUTOMATION_IDLE_TIMEOUT the scheduler reaps it.
# Poll state.json until the instance record disappears (the destroy job removed
# the container + record). Budget = idle timeout + destroy time, generously. Match
# the JSON-quoted name ("writer-...") so we key off the instance's `name` field
# exactly, not a workspace-path substring that may linger.
reaped=""
for _ in $(seq 1 60); do
  if ! grep -qF -- "\"${inst}\"" "${HOME}/.fleet/state.json" 2>/dev/null; then
    reaped=1
    break
  fi
  sleep 3
done
[ -n "${reaped}" ] || fail "idle automation instance was never reaped (still present after the idle timeout)"

pass "automation: fired trigger ran the agent with the prompt, then reaped the idle instance"
