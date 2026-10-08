#!/usr/bin/env bash
# Description: Fleet MCP inside instances (issue #219) — with the fleet's "Fleet
# MCP" setting on, the daemon serves its MCP server into every instance of the
# fleet and the instance's shells hand it to agents: a bash shell there gets
# CLAUDE_CODE_PLUGIN_DIRS pointing at a plugin (the MCP server + the
# fleet-admiral skill), and that server (`fleet mcp-bridge`) reaches the daemon —
# fleet_list answers with live data. Turning the setting off takes both away;
# turning it back on brings them back for a running instance.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test

# What an agent does with the plugin, as a script in the fixture repo (so it is
# in the instance's workspace): report the plugin its shell was handed, start
# the plugin's MCP server (the command in its .mcp.json) and call a tool.
cat > "${FIXTURE_REPO_DIR}/mcp-probe.sh" <<'PROBE'
echo "PLUGIN_DIRS=[${CLAUDE_CODE_PLUGIN_DIRS:-}]"
# The list holds other plugins too (the fleet status mod): pick the fleet's.
plugin=$(printf '%s\n' "${CLAUDE_CODE_PLUGIN_DIRS:-}" | tr ':' '\n' | grep '/fleet/mcp/claude-plugin$' | head -n 1)
[ -n "${plugin}" ] || exit 0
[ -f "${plugin}/skills/fleet-admiral/SKILL.md" ] && echo "SKILL_PRESENT"
cat "${plugin}/.mcp.json"
server=$(sed -n 's/.*"command": *"\([^"]*\)".*/\1/p' "${plugin}/.mcp.json")
{
  printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"itest","version":"1"}}}' \
    '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_list","arguments":{}}}'
  sleep 5
} | "${server}" mcp-bridge
echo "BRIDGE_EXIT=$?"
PROBE
( cd "${FIXTURE_REPO_DIR}" && git add -A && git commit -q -m "fixture: mcp probe" )

info "seeding the fleet with Fleet MCP on"
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
sock="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/alpha/.control/mcp.sock"

# probe runs the script from an interactive bash, the way a tmux session or an
# automation agent's launch does (it sources ~/.bashrc, hence fleet.rc).
probe() { "${FLEET_BIN}" exec "${inst}" -- bash -ic 'sh ./mcp-probe.sh' 2>/dev/null || true; }

# set_fleet_mcp <true|false> flips the setting in state.json (there is no CLI
# for fleet options) and waits for the daemon's reconcile to follow: the socket
# appears or goes. Re-applied each round in case the daemon rewrote the file.
set_fleet_mcp() {
  local want="$1"
  for _ in $(seq 1 30); do
    python3 - "${HOME}/.fleet/state.json" "${FIXTURE_REPO_NAME}" "${want}" <<'PY'
import json, os, sys
path, fleet, want = sys.argv[1], sys.argv[2], sys.argv[3] == "true"
with open(path) as f:
    st = json.load(f)
settings = st["fleets"][fleet].setdefault("settings", {})
if settings.get("fleetMcp", False) != want:
    settings["fleetMcp"] = want
    tmp = path + ".itest"
    with open(tmp, "w") as f:
        json.dump(st, f, indent=2)
    os.replace(tmp, path)
PY
    if [ "${want}" = "true" ] && [ -S "${sock}" ]; then return 0; fi
    if [ "${want}" = "false" ] && [ ! -e "${sock}" ]; then return 0; fi
    sleep 1
  done
  fail "the MCP socket did not follow fleetMcp=${want} (${sock})"
}

info "a new instance of the fleet has the socket and its shells the plugin"
[ -S "${sock}" ] || fail "no MCP socket in the instance's control directory (${sock})"
out=$(probe)
printf 'probe:\n%s\n' "${out}"
assert_contains "${out}" ".cache/fleet/mcp/claude-plugin" "the shell was not handed the fleet plugin via CLAUDE_CODE_PLUGIN_DIRS"
assert_contains "${out}" "SKILL_PRESENT" "the plugin is missing the fleet-admiral skill"
assert_contains "${out}" "mcp-bridge" "the plugin's .mcp.json does not run the bridge"
assert_contains "${out}" '"id":1' "no answer to the MCP initialize"
assert_contains "${out}" '\"instance\":\"alpha\"' "fleet_list through the bridge did not return the daemon's instances"
assert_contains "${out}" "BRIDGE_EXIT=0" "the bridge did not exit cleanly"

info "nothing was written to the agent's own config"
claude_json=$("${FLEET_BIN}" exec "${inst}" -- sh -c 'cat ~/.claude.json 2>/dev/null; ls ~/.claude/plugins 2>/dev/null; true' 2>/dev/null || true)
if printf '%s' "${claude_json}" | grep -q "fleet"; then
  fail "the fleet MCP was installed into the agent's config: ${claude_json}"
fi

info "turning the setting off takes the socket and the plugin away"
set_fleet_mcp false
out=$(probe)
assert_not_contains "${out}" ".cache/fleet/mcp/claude-plugin" "a shell still got the plugin with the setting off: ${out}"

info "turning it back on restores both for the running instance"
set_fleet_mcp true
out=$(probe)
assert_contains "${out}" ".cache/fleet/mcp/claude-plugin" "the plugin did not come back with the setting"
assert_contains "${out}" '\"instance\":\"alpha\"' "fleet_list did not answer after the setting came back"

pass "fleet MCP: the fleet setting serves the MCP to the agents in its instances"
