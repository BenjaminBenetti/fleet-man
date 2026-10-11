#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster whose network plugin enforces NetworkPolicies, an instance cannot reach the cloud metadata address 169.254.169.254 (proved against a stand-in metadata server on the node, which a pod outside the fleet reaches) while DNS still works; the deep test says so; turning block_metadata off reaches the running instance, which then gets there, and the deep test warns.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() {
  itest_backend_is kubernetes || return 0
  itest_k8s_kubectl delete pod metadata-stand-in other-workload --grace-period=0 --force --wait=false >/dev/null 2>&1 || true
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the cloud metadata block"

setup_test
alpha="${FIXTURE_REPO_NAME}/alpha"

info "the deep test: does the cluster's network plugin enforce NetworkPolicies?"
deep=$("${FLEET_BIN}" cluster test --deep itest 2>&1 || true)
printf '%s\n' "${deep}" | grep -E 'network policy|metadata' || true
if ! printf '%s\n' "${deep}" | grep -E '^network policy +pass' >/dev/null; then
  pass "skipped: this cluster's network plugin does not enforce NetworkPolicies (calico does: FLEET_QA_MINIKUBE_FLAGS=--cni=calico)"
fi
printf '%s\n' "${deep}" | grep -E '^metadata +pass .*not reachable from pods \(egress block on\)' >/dev/null \
  || fail "the deep test should find 169.254.169.254 unreachable under the egress block: ${deep}"

busybox="docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

info "a stand-in metadata server: 169.254.169.254 on the node's loopback"
itest_k8s_kubectl apply -f - >/dev/null <<POD
apiVersion: v1
kind: Pod
metadata: {name: metadata-stand-in}
spec:
  hostNetwork: true
  terminationGracePeriodSeconds: 0
  containers:
  - name: box
    image: ${busybox}
    securityContext: {privileged: true}
    command: [sh, -c, "ip addr add 169.254.169.254/32 dev lo 2>/dev/null || true; mkdir -p /tmp/www/latest && echo stand-in > /tmp/www/latest/index.html && exec httpd -f -p 169.254.169.254:80 -h /tmp/www"]
POD
itest_k8s_kubectl apply -f - >/dev/null <<POD
apiVersion: v1
kind: Pod
metadata: {name: other-workload, labels: {app: other}}
spec:
  terminationGracePeriodSeconds: 0
  containers:
  - {name: box, image: "${busybox}", command: [sleep, "3600"]}
POD
itest_k8s_kubectl wait --for=condition=Ready pod/metadata-stand-in pod/other-workload --timeout="$(_scale_timeout 180)s" >/dev/null \
  || fail "the stand-in pods never ran: $(itest_k8s_kubectl get pods -o wide 2>&1)"
got=$(itest_k8s_kubectl exec other-workload -- wget -q -T 5 -O- http://169.254.169.254/latest/ 2>&1 || true)
assert_equals "stand-in" "${got}" "a pod outside the fleet reaches the stand-in metadata server"

info "fleet up alpha"
fleet_up alpha
if out=$("${FLEET_BIN}" exec "${alpha}" -- curl -sS -m 5 http://169.254.169.254/latest/ 2>&1); then
  fail "alpha reached 169.254.169.254 under the metadata block: ${out}"
fi
"${FLEET_BIN}" exec "${alpha}" -- getent hosts kubernetes.default.svc.cluster.local >/dev/null \
  || fail "DNS does not work in alpha under the metadata block"

info "block_metadata off: the fleets already on the cluster lose their egress block"
"${FLEET_BIN}" cluster set itest block_metadata=false >/dev/null
got=""
deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  got=$("${FLEET_BIN}" exec "${alpha}" -- curl -sS -m 5 http://169.254.169.254/latest/ 2>/dev/null || true)
  [ "${got}" = "stand-in" ] && break
  sleep 2
done
assert_equals "stand-in" "${got}" "with block_metadata off alpha reaches the stand-in metadata server"
[ -z "$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" networkpolicies egress)" ] || fail "the egress policy is still there with block_metadata off"

deep=$("${FLEET_BIN}" cluster test --deep itest 2>&1 || true)
printf '%s\n' "${deep}" | grep -E '^metadata +warn .*169\.254\.169\.254 reachable from pods \(egress block off\)' >/dev/null \
  || fail "the deep test should warn that 169.254.169.254 is reachable with the block off: ${deep}"

pass "metadata blocked (stand-in proved reachable from outside the fleet), DNS kept, off switch honored, deep test agrees"
