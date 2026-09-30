#!/usr/bin/env bash
# Description: automation agent with the fleet MCP (issue #219) — a fired trigger
# launches an agent whose Fleet MCP is on; the launch is handed a Claude Code
# plugin (CLAUDE_CODE_PLUGIN_DIRS → .mcp.json + the fleet-admiral skill) for its
# process only, and the plugin's MCP server (`fleet mcp-bridge`) reaches the
# daemon through the instance's control socket: the fixture's fake `claude`
# calls fleet_list through it and gets the daemon's real answer.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
# Kill our daemon on exit so its short idle-timeout env can't bleed into a later
# test's scheduler (mirrors 391).
itest_cleanup() { pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true; }
itest_begin

# Short scheduler timings (see 391): the agent's instance is reaped a minute
# after it goes idle, which is well after the assertions below.
export FLEET_AUTOMATION_IDLE_TIMEOUT="60s"
export FLEET_AUTOMATION_TICK_INTERVAL="5s"

setup_fleetmcp_test

# Cold-spawn exactly one daemon with our env (see 391).
info "killing any lingering daemon so we cold-spawn one with our env"
pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
deadline=$(( $(date +%s) + $(_scale_timeout 5) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  [ "$(server_count)" = "0" ] && break
  sleep 0.2
done

# Fire on the daemon's startup tick: align to a fresh minute and seed a cron for
# the current one (see 391 for why).
sec="10#$(date +%S)"
if [ "$((sec))" -gt 5 ]; then sleep "$((60 - sec))"; fi
CRON="$(date +'%-M %-H') * * *"
info "seeding automation: agent 'orchestrator' (fleet MCP on) + schedule trigger ('${CRON}')"

cat > "${HOME}/.fleet/state.json" <<EOF
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {
      "name": "${FIXTURE_REPO_NAME}",
      "remote": "${FIXTURE_REPO_URL}",
      "settings": {
        "agents": [
          {
            "name": "orchestrator",
            "command": "claude --system-prompt '\${SYS_PROMPT}' '\${PROMPT}'",
            "backend": "devcontainer",
            "fleetMcp": true
          }
        ],
        "triggers": [
          {
            "name": "fire",
            "type": "schedule",
            "agentNames": ["orchestrator"],
            "cron": "${CRON}",
            "prompt": "split up the work"
          }
        ]
      },
      "instances": []
    }
  }
}
EOF

info "starting the daemon + scheduler ('fleet ls')"
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null 2>&1 || true

info "waiting for the scheduler to spawn an instance"
inst=""
for _ in $(seq 1 40); do
  inst=$(grep -oE 'orchestrator-[0-9]+-[0-9]+' "${HOME}/.fleet/state.json" 2>/dev/null | head -1 || true)
  [ -n "${inst}" ] && break
  sleep 3
done
[ -n "${inst}" ] || fail "scheduler never spawned an automation instance"
info "spawned instance: ${inst}"

# The fake claude writes its report once its MCP exchange is over.
info "waiting for the agent to launch and talk MCP to the daemon"
out=""
for _ in $(seq 1 80); do
  out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/${inst}" -- cat /tmp/fleetmcp_out 2>/dev/null || true)
  [ -n "${out}" ] && break
  sleep 3
done
printf 'agent report:\n%s\n' "${out}"

assert_contains "${out}" "AGENT_RAN" "fake claude was never executed (agent did not launch)"
assert_contains "${out}" "PLUGIN_DIRS=/tmp/fleet-mcp/claude-plugin" "the launch was not handed the fleet plugin via CLAUDE_CODE_PLUGIN_DIRS"
assert_contains "${out}" "SKILL_PRESENT" "the plugin is missing the fleet-admiral skill"
assert_contains "${out}" "mcp-bridge" "the plugin's .mcp.json does not run the bridge"
assert_contains "${out}" '"id":1' "no answer to the MCP initialize"
assert_contains "${out}" "${inst}" "fleet_list through the bridge did not return the daemon's instances"
assert_contains "${out}" "BRIDGE_EXIT=0" "the bridge did not exit cleanly"

# Handed to that launch only: a plain shell in the same instance has no plugin
# dir, and nothing was written to the fleet's shared Claude config.
info "checking the fleet MCP was not installed globally in the instance"
plain=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/${inst}" -- bash -ic 'echo "PLUGIN_DIRS=[${CLAUDE_CODE_PLUGIN_DIRS:-}]"' 2>/dev/null || true)
assert_contains "${plain}" "PLUGIN_DIRS=[]" "CLAUDE_CODE_PLUGIN_DIRS leaked beyond the agent's launch"

# The daemon serves the socket only into this instance's control directory.
sock="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/${inst}/.control/mcp.sock"
[ -S "${sock}" ] || fail "no MCP socket in the instance's control directory (${sock})"

pass "automation: an agent with the fleet MCP reached the daemon from inside its instance"
