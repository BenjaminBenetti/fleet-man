#!/usr/bin/env bash
# Description: Claude Code mount appears in the container at the expected path.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_agent_test
seed_fleet_settings "${FIXTURE_REPO_NAME}" true false /home/node

info "fleet up alpha (claude mount enabled)"
fleet_up alpha

info "asserting /home/node/.claude is a directory inside the container"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -d /home/node/.claude \
  || fail "/home/node/.claude is missing or not a directory"

info "asserting /home/node/.claude is a real bind mount"
mountinfo=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /proc/self/mountinfo)
assert_contains "${mountinfo}" " /home/node/.claude " "claude dir is not a mount point"

info "asserting /home/node/.claude.json symlink points into the shared files mount"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -L /home/node/.claude.json \
  || fail "/home/node/.claude.json is not a symlink"
link_target=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- readlink /home/node/.claude.json)
assert_contains "${link_target}" "/fleet-mounts/files/.claude.json" \
  "symlink target should land under /fleet-mounts/files"

info "asserting host-side fleet mount root exists"
assert_file_exists "${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/.claude"
assert_file_exists "${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/files/.claude.json"

pass "claude mount applied"
