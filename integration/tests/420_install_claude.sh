#!/usr/bin/env bash
# Description: Claude Code CLI is installed in the container by the startup script.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_agent_test
seed_fleet_settings "${FIXTURE_REPO_NAME}" true false /home/node

info "fleet up alpha (claude install enabled)"
fleet_up alpha

info "asserting startup script log was created"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -f /home/node/.fleet/startup/claude-code.log \
  || fail "claude-code.log was not created — startup runner did not execute"

info "claude-code.log contents:"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /home/node/.fleet/startup/claude-code.log || true

info "asserting claude binary is on the user's PATH"
# Use a login shell so ~/.local/bin (where the Anthropic installer
# drops the binary) is on PATH via the rc-file additions the installer
# made.
claude_path=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- bash -lc 'command -v claude' 2>&1) \
  || fail "claude is not on the user's PATH after install — output: ${claude_path}"
info "claude resolved at: ${claude_path}"
assert_contains "${claude_path}" "claude" "command -v claude returned no path"

pass "claude installed"
