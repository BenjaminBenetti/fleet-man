#!/usr/bin/env bash
# itest: k8s
# Description: single-node mode on a cluster of two or more nodes: a fleet's instances all run on the node holding its share, a new one created while every other is stopped included (the share volume's recorded topology, not a running pod, places it), and a share class that binds volumes Immediately is refused before anything is made. Needs two schedulable nodes and a WaitForFirstConsumer node-local default class (kind's local-path); elsewhere it passes as not run.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: a fleet's share placing its pods"

schedulable=$(itest_k8s_kubectl get nodes -o go-template='{{range .items}}{{$ok := true}}{{range .spec.taints}}{{if eq .effect "NoSchedule"}}{{$ok = false}}{{end}}{{end}}{{if $ok}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | grep -c . || true)
[ "${schedulable}" -ge 2 ] || pass "not run: needs two schedulable nodes (the cluster has ${schedulable})"
binding=$(itest_k8s_kubectl get storageclass -o go-template='{{range .items}}{{if eq (index .metadata.annotations "storageclass.kubernetes.io/is-default-class") "true"}}{{.volumeBindingMode}}{{end}}{{end}}')
[ "${binding}" = "WaitForFirstConsumer" ] || pass "not run: the default StorageClass binds ${binding:-nothing}, not WaitForFirstConsumer"
default_class=$(itest_k8s_kubectl get storageclass -o go-template='{{range .items}}{{if eq (index .metadata.annotations "storageclass.kubernetes.io/is-default-class") "true"}}{{.metadata.name}}{{end}}{{end}}')

setup_agent_test
# The share in the (node-local) default class: single-node mode, also on a
# cluster whose ReadWriteMany class Auto would pick (multi-node mode is 819's).
"${FLEET_BIN}" cluster set itest shared_storage_class="${default_class}" >/dev/null
other="itest-other"
cat > "${HOME}/.fleet/state.json" <<STATE
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {
      "name": "${FIXTURE_REPO_NAME}", "remote": "${FIXTURE_REPO_URL}",
      "settings": { "claudeCodeMount": true, "homeDir": "/home/node" },
      "instances": []
    },
    "${other}": {
      "name": "${other}", "remote": "${FIXTURE_REPO_URL}",
      "settings": { "claudeCodeMount": true, "homeDir": "/home/node" },
      "instances": []
    }
  }
}
STATE
alpha="${FIXTURE_REPO_NAME}/alpha"
beta="${FIXTURE_REPO_NAME}/beta"

run_in() { "${FLEET_BIN}" exec "$1" -- sh -c "$2"; }
node_of() { itest_k8s_kubectl get pod "$(itest_k8s_pod "$(itest_container_id "$1")")" -o 'jsonpath={.spec.nodeName}'; }

info "fleet up alpha: its seed binds the fleet's share beside its disk"
fleet_up alpha
share=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" persistentvolumeclaims share)
[ -n "${share}" ] || fail "the fleet has no share"
home=$(node_of "${alpha}")
topology=$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.metadata.annotations.fleet\.dev/share-topology}')
assert_contains "${topology}" "${home}" "the share's recorded topology names the node it is on"
run_in "${alpha}" 'echo kept > ~/.claude/kept.txt'

info "with every instance stopped, a new one still lands beside the share"
"${FLEET_BIN}" stop "${alpha}" >/dev/null
fleet_up beta
assert_equals "${home}" "$(node_of "${beta}")" "beta runs on the share's node though no pod holds it"
# Not luck: its pod and its seed's are required on that node.
beta_sts=$(itest_k8s_sts "$(itest_container_id "${beta}")")
assert_contains "$(itest_k8s_kubectl get statefulset "${beta_sts}" -o 'jsonpath={.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution}')" "${home}" "beta's pod is required on the share's node"
assert_contains "$(itest_k8s_kubectl get statefulset "${beta_sts}" -o 'jsonpath={.spec.template.spec.affinity.podAffinity}')" "fleet.dev/share-group" "beta's pod goes beside the share's running holders"
assert_contains "$(itest_k8s_kubectl get job "${beta_sts}-seed-g1" -o 'jsonpath={.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution}')" "${home}" "beta's seed was required on the share's node"
assert_equals "kept" "$(run_in "${beta}" 'cat ~/.claude/kept.txt')" "beta sees what alpha left"
"${FLEET_BIN}" start "${alpha}" >/dev/null
assert_equals "${home}" "$(node_of "${alpha}")" "alpha starts again beside the share"
assert_equals "$(run_in "${alpha}" 'stat -c %i ~/.claude/kept.txt')" "$(run_in "${beta}" 'stat -c %i ~/.claude/kept.txt')" "one inode for both"

info "a share class that binds Immediately on several nodes is refused, making nothing"
itest_k8s_kubectl apply -f - >/dev/null <<'SC'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fleet-itest-immediate
provisioner: rancher.io/local-path
volumeBindingMode: Immediate
reclaimPolicy: Delete
SC
trap 'itest_k8s_kubectl delete storageclass fleet-itest-immediate --ignore-not-found >/dev/null 2>&1 || true' EXIT
"${FLEET_BIN}" cluster set itest shared_storage_class=fleet-itest-immediate >/dev/null
"${FLEET_BIN}" cluster test itest >/dev/null 2>&1 || true
if out=$("${FLEET_BIN}" up "${other}/gamma" --repo "${FIXTURE_REPO_URL}" 2>&1); then
  fail "a fleet whose share would bind Immediately on several nodes was created: ${out}"
fi
assert_contains "${out}" "kubernetes:misconfigured:share_binding" "the refusal is coded"
assert_contains "${out}" "WaitForFirstConsumer" "the refusal names the fix"
[ -z "$(itest_k8s_fleet_object "${other}" persistentvolumeclaims share)" ] || fail "a refused share was made"

pass "single-node mode: the fleet together on its share's node, stopped siblings or not; Immediate binding refused"
