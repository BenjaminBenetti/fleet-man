#!/usr/bin/env bash
# itest: k8s
# Description: on a StorageClass whose volumes hold only their size and can grow (FLEET_ITEST_EXPAND_CLASS, as integration/clusters/lvm-localpv.sh makes one), a fleet's share kept from growing (auto_expand_disk off) fills: a pod's write past it fails, the upkeep warns, a host file too big for it pauses the sync with "share full"; with auto-expand on, the next upkeep grows the share by half (to a whole Gi), the volume really grows, and the paused file lands whole; a nearly full cache volume grows the same way; elsewhere it passes saying it did not run.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: growing a cluster's volumes"
class="${FLEET_ITEST_EXPAND_CLASS:-}"
[ -n "${class}" ] || pass "not run: FLEET_ITEST_EXPAND_CLASS names no capacity-enforcing expandable class (integration/nightly/expand.sh sets it)"
[ "$(itest_k8s_kubectl get storageclass "${class}" -o jsonpath='{.allowVolumeExpansion}' 2>/dev/null)" = true ] \
  || fail "StorageClass ${class} does not allow volume expansion"

# The upkeep (cache volumes and shares) every 15s, not every 30 minutes.
export FLEET_KUBE_CACHE_CHECK_INTERVAL=15s
setup_test
cat > "${HOME}/.fleet/state.json" <<STATE
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {
      "name": "${FIXTURE_REPO_NAME}", "remote": "${FIXTURE_REPO_URL}",
      "settings": { "homeDir": "/home/vscode", "customMounts": ["/opt/data"], "debCacheServer": true },
      "instances": []
    }
  }
}
STATE
# Instance disks stay on the cluster's default class (Auto could pick the
# expandable one for them too, where a 50Gi disk might not fit).
default_class=$(itest_k8s_kubectl get storageclass -o go-template='{{range .items}}{{if eq (index .metadata.annotations "storageclass.kubernetes.io/is-default-class") "true"}}{{.metadata.name}}{{end}}{{end}}')
[ -n "${default_class}" ] || pass "not run: the cluster has no default StorageClass for instance disks"
"${FLEET_BIN}" cluster set itest "storage_class=${default_class}" "shared_storage_class=${class}" "cache_storage_class=${class}" \
  share_size=1Gi cache_sizes.debcache=1Gi auto_expand_disk=false >/dev/null
itest_config_written
inst="${FIXTURE_REPO_NAME}/alpha"
fleet_up alpha
log="${HOME}/.fleet/fleet.log"
share=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" persistentvolumeclaims share)
[ -n "${share}" ] || fail "the fleet has no share"
# size <pvc> — its request, its capacity and its class.
size() { itest_k8s_kubectl get pvc "$1" -o 'jsonpath={.spec.resources.requests.storage} {.status.capacity.storage} {.spec.storageClassName}'; }
assert_equals "1Gi 1Gi ${class}" "$(size "${share}")" "the share before"
run_in() { "${FLEET_BIN}" exec "${inst}" -- sh -c "$1"; }

info "the share fills; a pod's write past it fails"
# As root: past the file system's reserve for root, to the upkeep's 90% of
# the volume's 1Gi.
itest_container_exec_as "$(itest_container_id "${inst}")" root sh -c 'dd if=/dev/zero of=/opt/data/fill bs=1M count=950 2>/dev/null; df -m /opt/data | tail -n 1'
if run_in 'dd if=/dev/zero of=/opt/data/over bs=1M count=400 2>/dev/null'; then
  fail "a 400 MiB write past a full 1Gi share succeeded: the class does not hold the volume's size"
fi
run_in 'rm -f /opt/data/over'
deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
until grep -q "kube share: the fleet's share is nearly full.*auto-expand is off" "${log}" 2>/dev/null; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the upkeep never warned of the nearly full share: $(grep 'kube share' "${log}" | tail -3)"
  sleep 2
done
assert_equals "1Gi 1Gi ${class}" "$(size "${share}")" "with auto-expand off the share stays"

info "a host file too big for the share: the sync pauses"
host_data=$(itest_share_dir "${FIXTURE_REPO_NAME}" .mnt/opt/data)
head -c $(( 300 * 1024 * 1024 )) /dev/urandom > "${host_data}/late.bin"
want=$(sha256sum "${host_data}/late.bin" | cut -d' ' -f1)
status=""
deadline=$(( $(date +%s) + $(_scale_timeout 240) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  status=$("${FLEET_BIN}" sync status "${FIXTURE_REPO_NAME}" 2>&1 || true)
  case "${status}" in *"share full"*) break ;; esac
  sleep 3
done
assert_contains "${status}" "share full" "fleet sync status names the full share"
if "${FLEET_BIN}" sync wait "${FIXTURE_REPO_NAME}" --timeout 10s >/dev/null 2>&1; then
  fail "fleet sync wait succeeded with the share full"
fi

info "auto-expand on: the next upkeep grows the share, and the paused file lands"
"${FLEET_BIN}" cluster set itest auto_expand_disk=true >/dev/null
deadline=$(( $(date +%s) + $(_scale_timeout 240) ))
until [ "$(size "${share}")" = "2Gi 2Gi ${class}" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the share did not grow to 2Gi: $(size "${share}"); $(grep 'kube share' "${log}" | tail -3)"
  sleep 3
done
grep -q "kube share: grew a nearly full share.*from=1Gi to=2Gi" "${log}" || fail "fleet.log does not record the share's growth"
size_mb=$(run_in 'df -m /opt/data | tail -n 1' | awk '{ print $2 }')
[ "${size_mb}" -gt 1500 ] || fail "the pod's file system did not grow with its volume: ${size_mb} MiB"
out=$("${FLEET_BIN}" sync wait "${FIXTURE_REPO_NAME}" --timeout 300s 2>&1) || fail "the sync did not catch up once the share grew: ${out}"
assert_equals "${want}" "$(run_in 'sha256sum /opt/data/late.bin' | cut -d' ' -f1)" "the paused file landed whole"
assert_not_contains "$("${FLEET_BIN}" sync status "${FIXTURE_REPO_NAME}" 2>&1)" "share full" "no pause once the share grew"

info "a nearly full cache volume grows too"
itest_wait_service "${FIXTURE_REPO_NAME}" debcache running 240
cache=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" persistentvolumeclaims aptcache)
dep=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" deployments aptcache)
[ -n "${cache}" ] && [ -n "${dep}" ] || fail "no deb cache volume or Deployment"
assert_equals "1Gi 1Gi ${class}" "$(size "${cache}")" "the deb cache's volume before"
# Past the upkeep's 90% of the volume's 1Gi.
itest_k8s_kubectl exec "deploy/${dep}" -- sh -c 'dd if=/dev/zero of=/var/cache/apt-cacher-ng/fill bs=1M count=950 2>/dev/null; df -m /var/cache/apt-cacher-ng | tail -n 1' || true
deadline=$(( $(date +%s) + $(_scale_timeout 240) ))
until [ "$(size "${cache}")" = "2Gi 2Gi ${class}" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the cache volume did not grow: $(size "${cache}"); $(grep 'kube caches' "${log}" | tail -3)"
  sleep 3
done
grep -q 'kube caches: grew a nearly full cache volume' "${log}" || fail "fleet.log does not record the cache volume's growth"

pass "on a capacity-enforcing class: the share fills, pauses the sync, grows by half with auto-expand on and the file lands; a cache volume grows too"
