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

# Project overrides share one per-fleet file, without replacing the repo's
# other .claude files. Use devcontainer exec's workspace, not a guessed path.
project_link=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- readlink .claude/settings.local.json)
assert_equals "/fleet-mounts/files/claude-settings.local.json" "${project_link}" "project overrides must point to fleet storage"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c 'printf "%s" "{\"permissions\":{\"allow\":[\"Bash(go test:*)\"]}}" > .claude/settings.local.json'
settings=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat .claude/settings.local.json)
assert_equals "${settings}" "$(cat "${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/files/claude-settings.local.json")" "project settings must persist on the host"

fleet_up beta
assert_equals "${settings}" "$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/beta" -- cat .claude/settings.local.json)" "new instances must share project overrides (#248)"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/beta" -- sh -c 'printf "%s" "{\"permissions\":{\"allow\":[\"Bash(go vet:*)\"]}}" > .claude/settings.local.json'
updated=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/beta" -- cat .claude/settings.local.json)
assert_equals "${updated}" "$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat .claude/settings.local.json)" "existing instances must see updates"

"${FLEET_BIN}" down "${FIXTURE_REPO_NAME}/alpha"
fleet_up alpha
assert_equals "${updated}" "$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat .claude/settings.local.json)" "project overrides must survive instance recreation"

pass "claude home and project mounts shared and persisted"
