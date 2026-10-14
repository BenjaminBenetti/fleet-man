#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, a pod substituted behind fleet's back (its StatefulSet's template changed, the pod replaced) is refused: no session reaches it, `fleet exec` fails kubernetes:tampered, and the TUI row says the pod was tampered with; destroy still cleans up.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: pod verification"

setup_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"
cid=$(itest_container_id "${inst}")
key=$(itest_k8s_key "${inst}")
sts=$(itest_k8s_sts "${cid}")
pod=$(itest_k8s_pod "${cid}")
assert_equals "ok" "$("${FLEET_BIN}" exec "${inst}" -- echo ok)" "exec before the tamper"
old_uid=$(itest_k8s_wait_pod_running "${pod}")

info "substituting the pod: a changed template, the pod replaced"
itest_k8s_kubectl patch statefulset "${sts}" --type=strategic \
  -p '{"spec":{"template":{"spec":{"containers":[{"name":"dev","env":[{"name":"ITEST_TAMPERED","value":"1"}]}]}}}}' >/dev/null
itest_k8s_kubectl delete pod "${pod}" --wait=false >/dev/null
new_uid=$(itest_k8s_wait_pod_running "${pod}" "${old_uid}")
info "the substitute pod ${new_uid} runs"
itest_k8s_kubectl get pod "${pod}" -o 'jsonpath={.spec.containers[?(@.name=="dev")].env}' | grep -q ITEST_TAMPERED \
  || fail "the substitute pod does not carry the changed template"

info "fleet refuses it"
out="" rc=0
deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  rc=0
  out=$("${FLEET_BIN}" exec "${inst}" -- echo reached 2>&1) || rc=$?
  if [ "${rc}" -ne 0 ] && printf '%s' "${out}" | grep -q 'kubernetes:tampered'; then
    break
  fi
  sleep 2
done
printf '%s\n' "${out}"
[ "${rc}" -ne 0 ] || fail "fleet exec reached the substituted pod: ${out}"
assert_contains "${out}" "kubernetes:tampered" "the refusal's code"
assert_contains "${out}" "is not the pod fleet made" "the refusal says why"
assert_not_contains "${out}" "reached" "nothing ran in the substituted pod"

info "the TUI row says so"
tui_spawn
tui_wait_for "alpha" 30 || fail "the TUI never listed alpha"
tui_wait_for "pod tampered with: refused" 60 || fail "the row does not say the pod was tampered with"

tui_kill
info "destroy cleans up"
"${FLEET_BIN}" down "${inst}" >/dev/null
itest_k8s_wait_no_objects "${key}" 180

pass "a substituted pod is refused (kubernetes:tampered) and shown as tampered with"
