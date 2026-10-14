#!/usr/bin/env bash
# itest: k8s
# Description: multi-node mode: on a cluster of two or more schedulable nodes with an NFS StorageClass (csi-driver-nfs), `fleet cluster test` runs the multi-node storage test and Auto picks the NFS class for fleet shares; the fleet's share is a ReadWriteMany volume of it, its pods are placed by nothing but the volume, siblings on different nodes see each other's writes at once, and Codex's SQLite state is each pod's own (CODEX_SQLITE_HOME below the user's home). Needs two schedulable nodes and a StorageClass of nfs.csi.k8s.io (FLEET_ITEST_CLUSTER); elsewhere it passes as not run.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: a fleet share mounted from several nodes"

schedulable=$(itest_k8s_kubectl get nodes -o go-template='{{range .items}}{{$ok := true}}{{if .spec.unschedulable}}{{$ok = false}}{{end}}{{range .spec.taints}}{{if eq .effect "NoSchedule"}}{{$ok = false}}{{end}}{{end}}{{if $ok}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | grep -c . || true)
[ "${schedulable}" -ge 2 ] || pass "not run: needs two schedulable nodes (the cluster has ${schedulable})"
nfs=$(itest_k8s_kubectl get storageclass -o go-template='{{range .items}}{{if eq .provisioner "nfs.csi.k8s.io"}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | head -1)
[ -n "${nfs}" ] || pass "not run: no StorageClass of nfs.csi.k8s.io (csi-driver-nfs)"

setup_agent_test
seed_fleet_settings "${FIXTURE_REPO_NAME}" true true /home/node
alpha="${FIXTURE_REPO_NAME}/alpha"
beta="${FIXTURE_REPO_NAME}/beta"

run_in() { "${FLEET_BIN}" exec "$1" -- sh -c "$2"; }
node_of() { itest_k8s_kubectl get pod "$(itest_k8s_pod "$(itest_container_id "$1")")" -o 'jsonpath={.spec.nodeName}'; }

info "fleet cluster test: the multi-node test passes ${nfs}, which Auto picks"
out=$("${FLEET_BIN}" cluster test itest 2>&1) || true
assert_contains "${out}" "auto → ${nfs}: multi-node mode" "the shared storage check names Auto's pick"
ls_out=$("${FLEET_BIN}" cluster ls --json 2>/dev/null || "${FLEET_BIN}" cluster ls)
info "cluster: $(printf '%s' "${ls_out}" | head -c 400)"

info "fleet up alpha, then beta while alpha's node is cordoned"
fleet_up alpha
home=$(node_of "${alpha}")
[ -n "${home}" ] || fail "alpha has no node"
itest_k8s_kubectl cordon "${home}" >/dev/null
trap 'itest_k8s_kubectl uncordon "${home}" >/dev/null 2>&1 || true' EXIT
fleet_up beta
itest_k8s_kubectl uncordon "${home}" >/dev/null
other=$(node_of "${beta}")
[ -n "${other}" ] && [ "${other}" != "${home}" ] || fail "beta runs on ${other:-no node}, beside alpha on ${home}"

info "the share: ReadWriteMany in ${nfs}, multi-node mode, placing nothing"
share=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" persistentvolumeclaims share)
[ -n "${share}" ] || fail "the fleet has no share"
assert_equals "multi-node" "$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.metadata.annotations.fleet\.dev/share-mode}')" "the share's mode"
assert_equals "${nfs}" "$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.spec.storageClassName}')" "the share's class"
assert_equals "ReadWriteMany" "$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.spec.accessModes[0]}')" "the share's access mode"
beta_sts=$(itest_k8s_sts "$(itest_container_id "${beta}")")
assert_equals "" "$(itest_k8s_kubectl get statefulset "${beta_sts}" -o 'jsonpath={.spec.template.spec.affinity.podAffinity}')" "no pod affinity to the share's holders"

info "siblings on two nodes see each other's writes at once"
run_in "${alpha}" 'echo from-alpha > ~/.claude/shared.txt'
assert_equals "from-alpha" "$(run_in "${beta}" 'cat ~/.claude/shared.txt')" "beta reads alpha's write"
run_in "${beta}" 'echo from-beta >> ~/.claude/shared.txt && mkdir -p ~/.codex/sessions && echo s > ~/.codex/sessions/x'
assert_equals "$(printf 'from-alpha\nfrom-beta')" "$(run_in "${alpha}" 'cat ~/.claude/shared.txt')" "alpha reads beta's append"
assert_equals "s" "$(run_in "${alpha}" 'cat ~/.codex/sessions/x')" "the Codex folder is shared too"

info "Codex's SQLite state stays on each pod's own disk"
for ref in "${alpha}" "${beta}"; do
  assert_equals "/home/node/.local/state/fleet/codex-sqlite" "$(run_in "${ref}" 'echo "${CODEX_SQLITE_HOME}"')" "${ref}: CODEX_SQLITE_HOME"
done

pass "multi-node mode: Auto's tested NFS class, one ReadWriteMany share across nodes, Codex's SQLite pod-local"
