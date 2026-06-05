#!/usr/bin/env bash
# Description: destroying a buildkit-enabled fleet removes the server container but preserves the build cache, which a re-created fleet reuses.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
# claude=false codex=false homedir=/home/vscode gh=false buildkit=true
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/vscode false true

bk_container="fleet-${FIXTURE_REPO_NAME}-buildkit"
host_bk_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/.buildkit"
cache_dir="${host_bk_dir}/cache"

wait_for_buildkit_running() {
  local running="missing"
  local i
  for i in $(seq 1 "$(_scale_timeout 30)"); do
    running=$(docker inspect -f '{{.State.Running}}' "${bk_container}" 2>/dev/null || echo missing)
    [ "${running}" = "true" ] && return 0
    sleep 1
  done
  fail "buildkit container ${bk_container} not running (state: ${running})"
}

info "fleet up alpha (buildkit enabled)"
fleet_up alpha
wait_for_buildkit_running

info "asserting the build cache populated under .buildkit/cache"
assert_file_exists "${cache_dir}"
[ -n "$(ls -A "${cache_dir}" 2>/dev/null)" ] \
  || fail "expected the buildkit cache to be non-empty after the server booted"

info "fleet destroy ${FIXTURE_REPO_NAME}"
"${FLEET_BIN}" destroy "${FIXTURE_REPO_NAME}"

info "asserting the server container is removed (no orphan / auto-restart)"
gone="no"
for _ in $(seq 1 "$(_scale_timeout 15)"); do
  if ! docker inspect "${bk_container}" >/dev/null 2>&1; then
    gone="yes"
    break
  fi
  sleep 1
done
[ "${gone}" = "yes" ] || fail "buildkit container ${bk_container} should be removed on destroy"

info "asserting the build cache is PRESERVED across destroy"
assert_file_exists "${cache_dir}"
[ -n "$(ls -A "${cache_dir}" 2>/dev/null)" ] \
  || fail "build cache should survive fleet destroy so it warms the next build"

info "re-creating the fleet brings the server back and reuses the kept cache"
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/vscode false true
fleet_up alpha
wait_for_buildkit_running
assert_file_exists "${cache_dir}"

pass "buildkit cache persists across destroy and is reused on re-create"
