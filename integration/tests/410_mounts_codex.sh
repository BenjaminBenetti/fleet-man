#!/usr/bin/env bash
# Description: Codex mount appears in the container at the expected path.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_agent_test
seed_fleet_settings "${FIXTURE_REPO_NAME}" false true /home/node

info "fleet up alpha (codex mount enabled)"
fleet_up alpha

info "asserting /home/node/.codex is a directory inside the container"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -d /home/node/.codex \
  || fail "/home/node/.codex is missing or not a directory"

info "asserting /home/node/.codex is a real bind mount"
mountinfo=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /proc/self/mountinfo)
assert_contains "${mountinfo}" " /home/node/.codex " "codex dir is not a mount point"

info "asserting host-side fleet mount root exists"
assert_file_exists "${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/.codex"

pass "codex mount applied"
