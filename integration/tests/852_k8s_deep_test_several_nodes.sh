#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster of several nodes, the deep test judges its storage with real pods: two pods on one node mount one ReadWriteOnce volume of the share's class (share pods passes), and fleets kept on a node-local class run each on one node, the connection LIMITED "each fleet on one node" with the fleet-size cap in its warning; on one node it passes saying it did not run.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the deep test on several nodes"
schedulable=$(itest_k8s_kubectl get nodes -o go-template='{{range .items}}{{$ok := not .spec.unschedulable}}{{range .spec.taints}}{{if eq .effect "NoSchedule"}}{{$ok = false}}{{end}}{{end}}{{if $ok}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | grep -c . || true)
[ "${schedulable}" -ge 2 ] || pass "not run: needs two schedulable nodes (the cluster has ${schedulable})"
local_class=""
for class in $(itest_k8s_kubectl get storageclass -o jsonpath='{.items[*].metadata.name}'); do
  case "$(itest_k8s_kubectl get storageclass "${class}" -o jsonpath='{.provisioner}')" in
    rancher.io/local-path | k8s.io/minikube-hostpath) local_class="${class}"; break ;;
  esac
done
[ -n "${local_class}" ] || pass "not run: no node-local StorageClass (local-path) on the cluster"

setup_test
info "fleets kept on ${local_class}, a class of node-local volumes"
"${FLEET_BIN}" cluster set itest "shared_storage_class=${local_class}" "storage_class=${local_class}" >/dev/null

info "the deep test"
deep=$("${FLEET_BIN}" cluster test --deep itest 2>&1 || true)
printf '%s\n' "${deep}" | grep -E '^(share pods|shared storage|attach limits) ' || true
printf '%s\n' "${deep}" | grep -Eq '^share pods +pass +two pods on one node mounted one ReadWriteOnce volume' \
  || fail "the two-pod probe did not pass: $(printf '%s\n' "${deep}" | grep -E '^share pods' || printf '%s' "${deep}")"
[ -z "$(itest_k8s_kubectl get pods,pvc -l fleet.dev/probe -o name 2>/dev/null)" ] || fail "the probes left pods or volumes behind"

info "each fleet on one node: LIMITED, with the fleet-size cap"
ls=$("${FLEET_BIN}" cluster ls itest 2>&1)
assert_contains "${ls}" "each fleet on one node" "the connection is LIMITED by single-node mode"
printf '%s\n' "${ls}" | grep -Eq "Each fleet's instances run together on one node \(cap [0-9]+\)" \
  || fail "the warning does not give the fleet-size cap: ${ls}"

pass "the deep test's two-pod probe passed on several nodes, and single-node mode is LIMITED with its cap"
