#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster the fleet's Buildkit toggle decides and the connection's allow_shared_buildkit permits: turned off, the fleet's server there goes, and an instance created or started there runs without it with its banner naming the setting; turned on, the server comes back and the running instances get it at once (a certificate, the banner line gone), no start needed; in a namespace whose pod-security level is baseline, shared Buildkit either runs or the instance's banner says the admission denies it, never a silent skip.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
NS=""
itest_cleanup() {
  if [ -n "${NS}" ]; then
    _be_kubectl delete namespace "${NS}" --wait=false >/dev/null 2>&1 || true
  fi
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: a cluster's shared Buildkit permission"

setup_test
# claude=false codex=false homedir=/home/vscode gh=false buildkit=true
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/vscode false true

warn_file() { printf '%s\n' "${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-$1.warn"; }
has_credential() { "${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/$1" -- sh -c 'test -s "$HOME/.fleet/buildkit/key.pem"' 2>/dev/null; }

info "fleet up alpha (buildkit on, the connection allows it)"
fleet_up alpha
itest_wait_service "${FIXTURE_REPO_NAME}" buildkit running 240
has_credential alpha || fail "alpha has no buildkit client certificate"

info "the connection stops allowing shared Buildkit: its server goes"
"${FLEET_BIN}" cluster set itest allow_shared_buildkit=false >/dev/null
itest_wait_service "${FIXTURE_REPO_NAME}" buildkit absent 120
[ -n "$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" persistentvolumeclaims buildkit)" ] \
  || fail "the server's cache volume went with the permission"

info "a create there runs without it and names the setting"
fleet_up beta
assert_contains "$(cat "$(warn_file beta)" 2>/dev/null)" "allow_shared_buildkit" "beta's banner names the connection's setting"
assert_contains "$(cat "${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-beta.log" 2>/dev/null)" "allow_shared_buildkit" "beta's creation log names the connection's setting"
if has_credential beta; then
  fail "beta got a buildkit certificate on a connection that does not allow shared Buildkit"
fi
assert_equals "absent" "$(itest_service_status "${FIXTURE_REPO_NAME}" buildkit)" "a create on a connection that does not allow it started its server"

info "a start there does too"
"${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/alpha"
"${FLEET_BIN}" start "${FIXTURE_REPO_NAME}/alpha"
assert_contains "$(cat "$(warn_file alpha)" 2>/dev/null)" "allow_shared_buildkit" "alpha's banner after its start names the setting"

info "allowed again: the server comes back, and the running instances get it now, with no start"
"${FLEET_BIN}" cluster set itest allow_shared_buildkit=true >/dev/null
itest_wait_service "${FIXTURE_REPO_NAME}" buildkit running 240
# beta was made while the connection refused the server; alpha started then.
deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
until has_credential beta && ! grep -q allow_shared_buildkit "$(warn_file alpha)" 2>/dev/null; do
  [ "$(date +%s)" -lt "${deadline}" ] \
    || fail "the running instances did not get shared Buildkit once it was allowed again (beta's certificate: $(has_credential beta && echo yes || echo no); alpha's banner: $(cat "$(warn_file alpha)" 2>/dev/null))"
  sleep 2
done
info "a start gives an instance a certificate too"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/beta" -- sh -c 'rm -rf "$HOME/.fleet/buildkit"'
"${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/beta"
"${FLEET_BIN}" start "${FIXTURE_REPO_NAME}/beta"
has_credential beta || fail "beta has no buildkit certificate after a start on a connection that allows it again"

info "a namespace at pod-security baseline: shared Buildkit runs, or the banner says the admission denies it"
NS="${FLEET_ITEST_NAMESPACE}-baseline"
_be_kubectl create namespace "${NS}" >/dev/null 2>&1 || true
_be_kubectl label --overwrite namespace "${NS}" pod-security.kubernetes.io/enforce=baseline >/dev/null
out=$("${FLEET_BIN}" cluster add "${FLEET_ITEST_CLUSTER}" --name baseline -n "${NS}" 2>&1) || fail "cluster add: ${out}"
"${FLEET_BIN}" cluster test baseline >/dev/null 2>&1 || true
up_out=$("${FLEET_BIN}" up gamma --repo "${FIXTURE_REPO_URL}" --cluster baseline 2>&1) || fail "fleet up gamma on the baseline namespace failed: ${up_out}"
banner=$(cat "$(warn_file gamma)" 2>/dev/null || true)
if printf '%s\n%s\n' "${up_out}" "${banner}" | grep -q "Shared Buildkit unavailable on this cluster: the namespace's admission denies its pod"; then
  assert_contains "${banner}" "admission denies its pod" "gamma's banner names the admission denial"
  info "the baseline namespace denies the server's pod, and gamma says so"
else
  ready=""
  for _ in $(seq 1 "$(_scale_timeout 240)"); do
    ready=$(_be_kubectl -n "${NS}" get deployments -l fleet.dev/service=buildkit -o 'jsonpath={.items[0].status.availableReplicas}' 2>/dev/null || true)
    [ "${ready:-0}" -ge 1 ] 2>/dev/null && break
    sleep 1
  done
  [ "${ready:-0}" -ge 1 ] 2>/dev/null || fail "no admission warning on gamma, and no shared Buildkit running in the baseline namespace: ${up_out}"
  "${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/gamma" -- sh -c 'test -s "$HOME/.fleet/buildkit/key.pem"' || fail "gamma has no buildkit certificate"
  info "the baseline namespace admits the server, and gamma has its certificate"
fi

pass "the connection's allow_shared_buildkit and its admission decide shared Buildkit there, and say so"
