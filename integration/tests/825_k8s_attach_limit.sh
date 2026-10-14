#!/usr/bin/env bash
# itest: k8s
# Description: the fleet-size cap of a node's volume attach limit: with a CSI driver that takes few volumes per node (csi-hostpath with --maxvolumespernode: integration/clusters/csi-hostpath-limit.sh), `fleet cluster test` names the limit and the cap it leaves a fleet in single-node mode (the fleet's disks and its share all attach to one node), a fleet fills it, and the instance past it stays Pending with the limit named in its creation log, then fails naming it. Needs a hostpath.csi.k8s.io StorageClass whose driver allows at most 6 volumes per node; elsewhere it passes as not run.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: CSI volume attach limits"

driver=hostpath.csi.k8s.io
class=$(itest_k8s_kubectl get storageclass -o go-template="{{range .items}}{{if eq .provisioner \"${driver}\"}}{{.metadata.name}}{{\"\\n\"}}{{end}}{{end}}" | head -1)
[ -n "${class}" ] || pass "not run: no StorageClass of ${driver}"
limit=$(itest_k8s_kubectl get csinode -o go-template="{{range .items}}{{range .spec.drivers}}{{if eq .name \"${driver}\"}}{{if .allocatable}}{{.allocatable.count}}{{end}}{{\"\\n\"}}{{end}}{{end}}{{end}}" | grep . | sort -n | head -1 || true)
[ -n "${limit}" ] && [ "${limit}" -le 6 ] || pass "not run: ${driver} allows ${limit:-any number of} volumes per node (integration/clusters/csi-hostpath-limit.sh sets a low limit)"
# The fleet's share is one of the node's volumes too.
cap=$((limit - 1))
[ "${cap}" -ge 1 ] || pass "not run: a limit of ${limit} leaves a fleet no room"

# The instance past the cap fails once the scheduler kept it unscheduled
# this long (fleetd inherits the environment).
export FLEET_KUBE_UNSCHEDULABLE_TIMEOUT=60s
setup_agent_test
"${FLEET_BIN}" cluster set itest storage_class="${class}" shared_storage_class="${class}" >/dev/null
seed_fleet_settings "${FIXTURE_REPO_NAME}" true false /home/node

info "fleet cluster test names the limit and the fleet-size cap"
out=$("${FLEET_BIN}" cluster test itest 2>&1) || true
info "$(printf '%s\n' "${out}" | grep -E '^(attach limits|shared storage)' -A1)"
assert_contains "${out}" "${limit} volumes per node (${driver})" "the attach limit"
assert_contains "${out}" "fleet-size cap (single-node mode): ${cap} instance" "the cap it leaves a fleet"
assert_contains "${out}" "cap ${cap} instance" "the shared storage check names the cap"

info "the fleet fills the node: ${cap} instance(s)"
for i in $(seq 1 "${cap}"); do
  fleet_up "fill${i}"
done

info "one more stays Pending, naming the limit"
past_out="${TEST_SCRATCH_DIR}/past.out"
log="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-past.log"
"${FLEET_BIN}" up past --repo "${FIXTURE_REPO_URL}" >"${past_out}" 2>&1 &
pid=$!
named="the node's volume attach limit is reached (${limit} ${driver} volumes per node)"
for _ in $(seq 1 120); do
  grep -qF "pending: ${named}" "${log}" 2>/dev/null && break
  kill -0 "${pid}" 2>/dev/null || break
  sleep 2
done
grep -qF "pending: ${named}" "${log}" 2>/dev/null || { cat "${log}" 2>/dev/null || true; cat "${past_out}"; fail "the creation log never named the attach limit"; }
assert_contains "$(grep -F "pending: ${named}" "${log}" | head -1)" "fleet-size cap ${cap}" "the pending step names the cap"
if wait "${pid}"; then
  fail "an instance past the fleet-size cap was created: $(cat "${past_out}")"
fi
past=$(cat "${past_out}")
assert_contains "${past}" "kubernetes:unschedulable" "the create fails unschedulable"
assert_contains "${past}" "${named}" "the failure names the limit"
assert_contains "${past}" "single-node mode" "and why the fleet's volumes share one node"

pass "the attach limit: the cap shown, the instance past it Pending and refused with the limit named"
