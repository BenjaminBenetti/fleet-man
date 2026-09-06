#!/usr/bin/env bash
# Description: Claude + Codex install + mount work together in a single instance.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_agent_test
seed_fleet_settings "${FIXTURE_REPO_NAME}" true true /home/node

info "fleet up alpha (claude + codex enabled)"
fleet_up alpha

# --- Mounts ---

info "asserting both directory mounts appear inside the container"
mountinfo=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /proc/self/mountinfo)
assert_contains "${mountinfo}" " /home/node/.claude "        "claude dir mount missing"
assert_contains "${mountinfo}" " /home/node/.codex "         "codex dir mount missing"
assert_contains "${mountinfo}" " /fleet-mounts/files "       "shared files mount missing"

info "asserting Claude file-mount symlink is in place"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -L /home/node/.claude.json \
  || fail "/home/node/.claude.json symlink missing"

# --- Installs ---

info "asserting both startup script logs were created"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -f /home/node/.fleet/startup/claude-code.log \
  || fail "claude-code.log missing"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -f /home/node/.fleet/startup/codex.log \
  || fail "codex.log missing"

info "asserting both binaries are on the user's PATH"
claude_path=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- bash -lc 'command -v claude' 2>&1) \
  || fail "claude not on PATH — output: ${claude_path}"
codex_path=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- bash -lc 'command -v codex' 2>&1) \
  || fail "codex not on PATH — output: ${codex_path}"
info "claude at: ${claude_path}"
info "codex  at: ${codex_path}"

info "asserting Codex can locate its bundled Code Mode host"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- bash -lc '
  set -e
  binary=$(readlink -f "$(command -v codex)")
  test -x "$(dirname "$binary")/codex-code-mode-host"
  test ! -e "$HOME/.codex/packages/standalone"
  codex --version
' || fail "Codex Code Mode host is missing or the install used the shared mount"

pass "claude + codex install + mount"
