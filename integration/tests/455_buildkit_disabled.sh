#!/usr/bin/env bash
# Description: with buildkit_server disabled, no buildkit container or socket is created and nothing is mounted into the instance.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
# claude=false codex=false homedir=/home/vscode gh=false buildkit=false
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/vscode false false

bk_container="fleet-${FIXTURE_REPO_NAME}-buildkit"
host_bk_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/.buildkit"

info "fleet up alpha (buildkit disabled)"
fleet_up alpha

info "asserting no shared buildkit container was created"
if docker inspect "${bk_container}" >/dev/null 2>&1; then
  fail "buildkit container ${bk_container} should not exist when the setting is off"
fi

info "asserting no host .buildkit directory was created"
assert_file_absent "${host_bk_dir}"

info "asserting nothing is mounted at /run/fleet-buildkit inside the instance"
if "${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -e /run/fleet-buildkit/buildkitd.sock 2>/dev/null; then
  fail "buildkit socket should not be present inside the instance when disabled"
fi

pass "buildkit disabled: no server, no socket, no mount"
