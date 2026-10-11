#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, a fleet's instances resolve each other by DNS (FLEET_PEER_DOMAIN) under the fleet's headless Service, beside its NetworkPolicies; its deb cache runs as a Deployment whose volume shows on the Caching row and which Delete cache remakes; destroying the fleet's last instance there deletes its objects but keeps the cache volume, which warms the next instance.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: a fleet's own objects on a cluster"

setup_test
cat > "${HOME}/.fleet/state.json" <<STATE
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {
      "name": "${FIXTURE_REPO_NAME}",
      "remote": "${FIXTURE_REPO_URL}",
      "settings": { "debCacheServer": true },
      "instances": []
    }
  }
}
STATE
alpha="${FIXTURE_REPO_NAME}/alpha"
beta="${FIXTURE_REPO_NAME}/beta"

# fleet_object <kind> <suffix> — the fleet's object's name, failing the test
# when there is none.
fleet_object() {
  local name
  name=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" "$1" "$2")
  [ -n "${name}" ] || fail "the fleet has no $1 ...-$2 on the cluster: $(itest_k8s_fleet_objects "${FIXTURE_REPO_NAME}" | tr '\n' ' ')"
  printf '%s\n' "${name}"
}
# volume_uid — the deb cache volume's uid.
volume_uid() { itest_k8s_kubectl get pvc "$(fleet_object persistentvolumeclaims aptcache)" -o 'jsonpath={.metadata.uid}'; }

info "fleet up alpha + beta (deb cache on)"
fleet_up alpha
fleet_up beta

info "the fleet's objects: its anchor, headless Service and NetworkPolicies"
anchor=$(fleet_object configmaps anchor)
net=$(fleet_object services net)
for p in instances caches egress; do fleet_object networkpolicies "${p}" >/dev/null; done
owner=$(itest_k8s_kubectl get service "${net}" -o 'jsonpath={.metadata.ownerReferences[0].name}')
assert_equals "${anchor}" "${owner}" "the headless Service hangs off the fleet's anchor"
alpha_sts=$(itest_k8s_sts "$(itest_container_id "${alpha}")")
assert_equals "${net}" "$(itest_k8s_kubectl get statefulset "${alpha_sts}" -o 'jsonpath={.spec.serviceName}')" "the instances' StatefulSets name the fleet's headless Service"
egress=$(itest_k8s_kubectl get networkpolicy "$(fleet_object networkpolicies egress)" -o 'jsonpath={.spec.egress[0].to[0].ipBlock.except[0]}')
assert_equals "169.254.0.0/16" "${egress}" "the egress policy blocks the cloud metadata range"
itest_wait_service "${FIXTURE_REPO_NAME}" debcache running 240

info "sibling DNS: alpha resolves beta's pod under FLEET_PEER_DOMAIN"
domain=$("${FLEET_BIN}" exec "${alpha}" -- bash -c '. "$HOME/.fleet/fleet.rc" >/dev/null 2>&1; printf "%s" "${FLEET_PEER_DOMAIN:-}"')
info "FLEET_PEER_DOMAIN=${domain}"
case "${domain}" in
  "${net}.${FLEET_ITEST_NAMESPACE}.svc."?*) ;;
  *) fail "FLEET_PEER_DOMAIN is '${domain}', want ${net}.${FLEET_ITEST_NAMESPACE}.svc.<cluster domain>" ;;
esac
beta_pod=$(itest_k8s_pod "$(itest_container_id "${beta}")")
beta_ip=$(itest_k8s_kubectl get pod "${beta_pod}" -o 'jsonpath={.status.podIP}')
resolved=$("${FLEET_BIN}" exec "${alpha}" -- getent hosts "${beta_pod}.${domain}" | awk '{print $1; exit}')
assert_equals "${beta_ip}" "${resolved}" "alpha resolves ${beta_pod}.${domain} to beta's pod"

info "an instance reaches the fleet's deb cache Service"
apt_svc=$(fleet_object services aptcache)
"${FLEET_BIN}" exec "${alpha}" -- curl -fsS -m 15 -o /dev/null "http://${apt_svc}:3142/acng-report.html" \
  || fail "alpha could not reach the deb cache at http://${apt_svc}:3142"

info "the Caching row shows the deb cache's volume on the cluster"
before=$(volume_uid)
tui_spawn
tui_wait_for "${FIXTURE_REPO_NAME}" 15 || fail "the TUI never rendered the fleet"
tui_send e
tui_wait_for "Edit fleet" 5 || fail "the fleet options dialog did not open"
for _ in $(seq 1 30); do
  tui_capture | grep -qF "> ▶ Caching" && break
  tui_send j
  sleep 0.1
done
tui_send l
# The usage lines, not the instance rows' cluster label: "☸ itest <used> of
# <size> (<n>%)".
tui_wait_for "of 10.0 GiB (" 60 || { tui_capture >&2; fail "the Caching rows show no cluster volume"; }
tui_capture | grep -E '☸ itest [0-9.]+ [KMG]?i?B of 10\.0 GiB \([0-9]+%\)' >/dev/null \
  || { tui_capture >&2; fail "the deb cache's row does not show its 10Gi volume on connection itest"; }
tui_send Escape
tui_wait_for_absent "Edit fleet" 5 || fail "the fleet options dialog did not close"

info "Delete cache remakes the cluster's deb cache volume and server"
tui_delete_fleet_cache "Deb package cache" "Deb cache cleared"
tui_kill
after=$(volume_uid)
[ -n "${after}" ] && [ "${after}" != "${before}" ] || fail "Delete cache kept the volume (uid ${before} -> ${after})"
itest_wait_service "${FIXTURE_REPO_NAME}" debcache running 120

info "destroying beta leaves the fleet's objects for alpha"
"${FLEET_BIN}" down "${beta}" >/dev/null
[ -n "$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" configmaps anchor)" ] || fail "the anchor went with an instance the fleet still has siblings of"
itest_wait_service "${FIXTURE_REPO_NAME}" debcache running 30

info "destroying alpha, the fleet's last instance there, deletes its objects but the cache volume"
"${FLEET_BIN}" down "${alpha}" >/dev/null
deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
left=""
while [ "$(date +%s)" -lt "${deadline}" ]; do
  left=$(itest_k8s_fleet_objects "${FIXTURE_REPO_NAME}")
  [ "${left}" = "persistentvolumeclaim/$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" persistentvolumeclaims aptcache)" ] && break
  sleep 2
done
assert_equals "persistentvolumeclaim/$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" persistentvolumeclaims aptcache)" "${left}" "only the cache volume outlives the fleet's last instance"

info "the next instance finds the cache volume warm"
fleet_up gamma
assert_equals "${after}" "$(volume_uid)" "the cache volume is the one the fleet had"
fleet_object configmaps anchor >/dev/null
itest_wait_service "${FIXTURE_REPO_NAME}" debcache running 240

pass "fleet objects: sibling DNS, policies, cache usage and wipe, teardown on the last instance, warm cache volume"
