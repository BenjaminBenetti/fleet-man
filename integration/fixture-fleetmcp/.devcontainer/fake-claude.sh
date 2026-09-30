#!/bin/sh
# Stand-in for Claude Code in the fleet-MCP test (393). It records what fleet
# handed this launch — CLAUDE_CODE_PLUGIN_DIRS and the plugin in it — then does
# what Claude would: starts the plugin's MCP server (the command in its
# .mcp.json) and talks MCP to it, recording the daemon's answers. Then idles.
out=/tmp/fleetmcp_out
{
  echo "AGENT_RAN"
  echo "PLUGIN_DIRS=${CLAUDE_CODE_PLUGIN_DIRS}"
  plugin="${CLAUDE_CODE_PLUGIN_DIRS%%:*}"
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
} > "${out}.tmp" 2>&1
mv "${out}.tmp" "${out}"
exec sleep 600
