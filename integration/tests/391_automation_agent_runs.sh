#!/usr/bin/env bash
# Description: end-to-end automation (issue #188) — a fired Schedule trigger
# spawns an instance, launches the agent, and the agent actually RUNS with the
# trigger's prompt. The fixture installs a fake `claude` under ~/.local/bin (on
# PATH only via .bashrc, like the real install) that records its args to
# /tmp/automation_agent_out; we assert the prompt reaches it. This is the real
# proof the scheduler -> spawn -> launch chain works.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_automation_test

MARKER="AUTOMATIONPROMPTMARKER42"

# A cron matching the CURRENT minute fires once (next match is tomorrow), so the
# daemon's immediate startup tick fires it without spamming a new instance every
# minute. Guard the minute-rollover race: if we're near the end of a minute, wait
# for the next one so the cron stays valid for ~a full minute.
if [ "$(date +%S)" -gt 45 ]; then sleep 16; fi
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

pass "automation: fired trigger spawned an instance and launched the agent with the prompt"
