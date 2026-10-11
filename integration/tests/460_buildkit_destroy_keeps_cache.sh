#!/usr/bin/env bash
# Description: destroying a buildkit-enabled fleet removes the server but preserves the build cache (a host dir on docker, a cache volume on a cluster), which a re-created fleet reuses; on a cluster the fleet's CA goes with it, so the new server takes no certificate of the old one's.
# itest: k8s
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
# claude=false codex=false homedir=/home/vscode gh=false buildkit=true
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/vscode false true

bk_container="fleet-${FIXTURE_REPO_NAME}-buildkit"
cache_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/.buildkit/cache"

# cache_volume — the cluster cache volume's uid.
cache_volume() {
  local pvc
  pvc=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" persistentvolumeclaims buildkit)
  [ -n "${pvc}" ] || fail "the fleet has no buildkit cache volume"
  itest_k8s_kubectl get pvc "${pvc}" -o 'jsonpath={.metadata.uid}'
}
# server_ca — the CA the cluster server takes client certificates of.
server_ca() {
  itest_k8s_kubectl get secret "$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" secrets buildkit)" -o 'jsonpath={.data.ca\.pem}'
}

info "fleet up alpha (buildkit enabled)"
fleet_up alpha
itest_wait_service "${FIXTURE_REPO_NAME}" buildkit running 240

if itest_backend_is kubernetes; then
  volume=$(cache_volume)
  ca=$(server_ca)
  [ -n "${ca}" ] || fail "the buildkit Secret holds no CA"
else
  info "asserting the build cache populated under .buildkit/cache"
  assert_file_exists "${cache_dir}"
  [ -n "$(ls -A "${cache_dir}" 2>/dev/null)" ] \
    || fail "expected the buildkit cache to be non-empty after the server booted"
fi

info "fleet destroy ${FIXTURE_REPO_NAME}"
"${FLEET_BIN}" destroy "${FIXTURE_REPO_NAME}"

info "asserting the server is removed (no orphan / auto-restart)"
itest_wait_service "${FIXTURE_REPO_NAME}" buildkit absent 60

info "asserting the build cache is PRESERVED across destroy"
if itest_backend_is kubernetes; then
  assert_equals "${volume}" "$(cache_volume)" "the cache volume should survive fleet destroy so it warms the next build"
  ca_files=$(ls "${HOME}/.fleet/kube/$(itest_k8s_connection)/buildkit" 2>/dev/null || true)
  assert_equals "" "${ca_files}" "the fleet's Buildkit CA should go with the fleet"
else
  assert_file_exists "${cache_dir}"
  [ -n "$(ls -A "${cache_dir}" 2>/dev/null)" ] \
    || fail "build cache should survive fleet destroy so it warms the next build"
  if docker inspect "${bk_container}" >/dev/null 2>&1; then
    fail "buildkit container ${bk_container} should be removed on destroy"
  fi
fi

info "re-creating the fleet brings the server back and reuses the kept cache"
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/vscode false true
fleet_up alpha
itest_wait_service "${FIXTURE_REPO_NAME}" buildkit running 240
if itest_backend_is kubernetes; then
  assert_equals "${volume}" "$(cache_volume)" "the re-created fleet's server should reuse the kept cache volume"
  [ "$(server_ca)" != "${ca}" ] || fail "the re-created fleet's server takes the destroyed fleet's CA"
else
  assert_file_exists "${cache_dir}"
fi

pass "buildkit cache persists across destroy and is reused on re-create"
