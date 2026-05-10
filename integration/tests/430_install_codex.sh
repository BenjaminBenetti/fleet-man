#!/usr/bin/env bash
# Description: Codex CLI is installed in the container by the startup script.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_agent_test
seed_fleet_settings "${FIXTURE_REPO_NAME}" false true /home/node

info "fleet up alpha (codex install enabled)"
fleet_up alpha

info "asserting startup script log was created"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -f /home/node/.fleet/startup/codex.log \
  || fail "codex.log was not created — startup runner did not execute"

info "codex.log contents:"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /home/node/.fleet/startup/codex.log || true

info "asserting codex binary is on the user's PATH"
codex_path=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- bash -lc 'command -v codex' 2>&1) \
  || fail "codex is not on the user's PATH after install — output: ${codex_path}"
info "codex resolved at: ${codex_path}"
assert_contains "${codex_path}" "codex" "command -v codex returned no path"

pass "codex installed"
