#!/usr/bin/env bash
# Description: Auggie CLI is installed in the container by the startup script.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_agent_test
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/node false false "" true

info "fleet up alpha (auggie install enabled)"
fleet_up alpha

info "asserting startup script log was created"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -f /home/node/.fleet/startup/auggie.log \
  || fail "auggie.log was not created — startup runner did not execute"

info "auggie.log contents:"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /home/node/.fleet/startup/auggie.log || true

info "asserting auggie binary is on the user's PATH"
auggie_path=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- bash -lc 'command -v auggie' 2>&1) \
  || fail "auggie is not on the user's PATH after install — output: ${auggie_path}"
info "auggie resolved at: ${auggie_path}"
assert_contains "${auggie_path}" "auggie" "command -v auggie returned no path"

pass "auggie installed"
