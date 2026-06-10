#!/usr/bin/env bash
# Description: Launching the TUI registers the fleet MCP server in ~/.claude.json.
set -euo pipefail

source "$(dirname "$0")/../common.sh"

CLAUDE_JSON="${HOME}/.claude.json"
CLAUDE_JSON_BAK="${CLAUDE_JSON}.itest-bak"

# BACKUP_DONE guards the cleanup: only remove the config we observed being
# created if this run actually got as far as making the backup decision.
BACKUP_DONE=""

itest_cleanup() {
  tui_kill
  # Put the user's real config back exactly as we found it.
  if [ -f "${CLAUDE_JSON_BAK}" ]; then
    mv "${CLAUDE_JSON_BAK}" "${CLAUDE_JSON}"
  elif [ -n "${BACKUP_DONE}" ]; then
    rm -f "${CLAUDE_JSON}"
  fi
}
itest_begin

# A leftover .itest-bak from a hard-killed previous run holds the user's REAL
# config — restore it first, never delete it. Then preserve any current config
# and start without one, so we observe the install create it from scratch.
if [ -f "${CLAUDE_JSON_BAK}" ]; then
  mv "${CLAUDE_JSON_BAK}" "${CLAUDE_JSON}"
fi
if [ -f "${CLAUDE_JSON}" ]; then
  mv "${CLAUDE_JSON}" "${CLAUDE_JSON_BAK}"
fi
BACKUP_DONE=1

setup_test

# The install is explicitly skipped when the client points at a remote daemon
# (admiralmcp.EnsureInstalledEventually); neutralize any ambient remote config
# so the spawned TUI exercises the local path.
unset FLEET_GATEWAY FLEET_SERVER

# No fleet_up needed: the install fires from tui.Run(), but unlike the skill
# install it is asynchronous — it waits for the auto-spawned daemon to publish
# ~/.fleet/mcp.port + mcp.token, then merges the entry. The binary's own
# deadline is a fixed 30s (internal/tui/app.go), so the poll budget matches it
# plus headroom rather than scaling with FLEET_TUI_WAIT_SCALE.
info "Spawning TUI"
tui_spawn

info "Waiting for fleet MCP server to be registered in ~/.claude.json"
deadline=$(( $(date +%s) + $(_scale_timeout 35) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  grep -q '"fleet"' "${CLAUDE_JSON}" 2>/dev/null && break
  sleep 0.25
done

if ! grep -q '"fleet"' "${CLAUDE_JSON}" 2>/dev/null; then
  {
    echo '--- poll timed out; ~/.fleet ---'
    ls -la "${HOME}/.fleet" 2>/dev/null || true
  } >&2
fi

# Distinguish "daemon never published its endpoint" from "installer never
# wrote the config" before asserting on the config itself.
assert_file_exists "${HOME}/.fleet/mcp.port"
assert_file_exists "${HOME}/.fleet/mcp.token"
assert_file_exists "${CLAUDE_JSON}"

# Assert via exit-code-only greps: the config embeds the live bearer token,
# which must never be echoed into test logs on failure.
grep -qF '"mcpServers"' "${CLAUDE_JSON}"   || fail "config missing mcpServers"
grep -qF '"fleet"' "${CLAUDE_JSON}"        || fail "config missing fleet server entry"
grep -qF '"type": "http"' "${CLAUDE_JSON}" || fail "fleet entry is not an http server"

# The registered endpoint must match the daemon's live discovery files: the
# loopback URL on the bound port, and the persistent bearer token.
port="$(cat "${HOME}/.fleet/mcp.port")"
token="$(cat "${HOME}/.fleet/mcp.token")"
grep -qF "\"url\": \"http://127.0.0.1:${port}\"" "${CLAUDE_JSON}" \
  || fail "registered URL does not match mcp.port (port=${port})"
grep -qF "Bearer ${token}" "${CLAUDE_JSON}" \
  || fail "registered Authorization header does not match mcp.token"

pass "TUI startup registers the fleet MCP server in ~/.claude.json"
