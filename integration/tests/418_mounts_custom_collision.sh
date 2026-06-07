#!/usr/bin/env bash
# Description: a custom mount whose container path collides with a managed
# mount target (Claude) must not crash `fleet up` with a "Duplicate mount
# point" error. The resolver dedups by container path keeping the custom
# mount, so the custom mount wins (the documented last-wins behavior).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_agent_test
# claude=true, codex=false, homedir=/home/node, gh=false, buildkit=false
# customMounts collides with the managed Claude mount target.
seed_fleet_settings "${FIXTURE_REPO_NAME}" true false /home/node false false '["/home/node/.claude"]'

info "seeding the custom mount's host dir with a winner marker"
custom_host="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/.mnt/home/node/.claude"
mkdir -p "${custom_host}"
printf "custom\n" > "${custom_host}/WINNER.txt"

info "fleet up alpha (custom mount collides with managed Claude mount)"
# fleet_up fails the test if provisioning exits non-zero — this is the
# regression guard for the "Duplicate mount point" crash.
fleet_up alpha

info "asserting /home/node/.claude is a directory inside the container"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -d /home/node/.claude \
  || fail "/home/node/.claude is missing or not a directory"

info "asserting the CUSTOM mount won the collision (marker is visible)"
winner=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /home/node/.claude/WINNER.txt)
assert_contains "${winner}" "custom" \
  "expected the custom mount to win the collision (last-wins), but the marker was absent"

pass "colliding custom mount wins without crashing fleet up"
