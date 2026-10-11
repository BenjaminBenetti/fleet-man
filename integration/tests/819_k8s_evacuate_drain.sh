#!/usr/bin/env bash
# itest: k8s
# Description: a fleet in single-node mode leaves a node the cluster empties: draining the node its share's holders run on moves the fleet as one (stopped there, started together on another node, the connection saying "node cordoned: fleet moving" meanwhile), an instance protected from drains (a PodDisruptionBudget the drain cannot pass) included, the files on its share and on each disk kept, a stopped sibling started afterwards beside them, and a move fleetd is killed in the middle of finished by the next fleetd; a fleet whose share is on the node's own disk is left running there, the connection saying why it can't move. Needs two schedulable nodes and a StorageClass whose volumes any node mounts (FLEET_ITEST_MOVABLE_CLASS, default fleet-shared-path, as integration/clusters/kind-shared-path.sh makes); elsewhere it passes as not run.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: a fleet moved off a drained node"

schedulable=$(itest_k8s_kubectl get nodes -o go-template='{{range .items}}{{$ok := not .spec.unschedulable}}{{range .spec.taints}}{{if eq .effect "NoSchedule"}}{{$ok = false}}{{end}}{{end}}{{if $ok}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | grep -c . || true)
[ "${schedulable}" -ge 2 ] || pass "not run: needs two schedulable nodes (the cluster has ${schedulable})"
class="${FLEET_ITEST_MOVABLE_CLASS:-fleet-shared-path}"
itest_k8s_kubectl get storageclass "${class}" >/dev/null 2>&1 \
  || pass "not run: no StorageClass ${class} whose volumes every node mounts (integration/clusters/kind-shared-path.sh makes one)"

setup_agent_test
pinned="itest-pinned"
cat > "${HOME}/.fleet/state.json" <<STATE
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {
      "name": "${FIXTURE_REPO_NAME}", "remote": "${FIXTURE_REPO_URL}",
      "settings": { "claudeCodeMount": true, "homeDir": "/home/node" },
      "instances": []
    },
    "${pinned}": {
      "name": "${pinned}", "remote": "${FIXTURE_REPO_URL}",
      "settings": { "claudeCodeMount": true, "homeDir": "/home/node" },
      "instances": []
    }
  }
}
STATE
alpha="${FIXTURE_REPO_NAME}/alpha"
beta="${FIXTURE_REPO_NAME}/beta"
gamma="${FIXTURE_REPO_NAME}/gamma"
log="${HOME}/.fleet/fleet.log"

run_in() { "${FLEET_BIN}" exec "$1" -- sh -c "$2"; }
node_of() { itest_k8s_kubectl get pod "$(itest_k8s_pod "$(itest_container_id "$1")")" -o 'jsonpath={.spec.nodeName}'; }
uid_of() { itest_k8s_kubectl get pod "$(itest_k8s_pod "$(itest_container_id "$1")")" -o 'jsonpath={.metadata.uid}'; }
status_lines() { "${FLEET_BIN}" cluster ls itest 2>/dev/null || true; }
# wait_status <pattern> <present|absent> <what> [timeout_s]
wait_status() {
  local deadline=$(( $(date +%s) + $(_scale_timeout "${4:-120}") ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    if status_lines | grep -qF -- "$1"; then
      [ "$2" = present ] && return 0
    else
      [ "$2" = absent ] && return 0
    fi
    sleep 1
  done
  fail "$3: $(status_lines | tr '\n' ' ')"
}
cordoned=()
uncordon_all() {
  local n
  for n in ${cordoned[@]+"${cordoned[@]}"}; do itest_k8s_kubectl uncordon "${n}" >/dev/null 2>&1 || true; done
}
trap uncordon_all EXIT

info "a fleet whose share is on its node's own disk (the default class) stays put"
"${FLEET_BIN}" up "${pinned}/alpha" --repo "${FIXTURE_REPO_URL}" >/dev/null
pinned_node=$(node_of "${pinned}/alpha")
pinned_uid=$(uid_of "${pinned}/alpha")
cordoned+=("${pinned_node}")
itest_k8s_kubectl cordon "${pinned_node}" >/dev/null
wait_status "node cordoned: fleet can't move (${pinned} stays on ${pinned_node}: its share is node-local" present "the connection never said the pinned fleet can't move"
assert_equals "${pinned_uid}" "$(uid_of "${pinned}/alpha")" "the pinned fleet's pod was left running"
grep -q 'kube evacuate: the fleet cannot move off the node the cluster empties' "${log}" || fail "fleet.log does not say why the fleet stays"
itest_k8s_kubectl uncordon "${pinned_node}" >/dev/null
wait_status "fleet can't move" absent "the warning outlived the cordon"
"${FLEET_BIN}" down "${pinned}/alpha" >/dev/null

info "a fleet on ${class}: alpha and beta (protected from drains) running, gamma stopped"
"${FLEET_BIN}" cluster set itest "storage_class=${class}" "shared_storage_class=${class}" >/dev/null
"${FLEET_BIN}" cluster test itest >/dev/null 2>&1 || true
fleet_up alpha
# beta's PodDisruptionBudget refuses every eviction: the drain cannot empty
# the node of it, only the move can.
"${FLEET_BIN}" cluster set itest protect_from_drain=true >/dev/null
"${FLEET_BIN}" cluster test itest >/dev/null 2>&1 || true
fleet_up beta
fleet_up gamma
itest_k8s_kubectl get poddisruptionbudget "$(itest_k8s_sts "$(itest_container_id "${beta}")")" >/dev/null \
  || fail "beta has no PodDisruptionBudget"
if itest_k8s_kubectl get poddisruptionbudget "$(itest_k8s_sts "$(itest_container_id "${alpha}")")" >/dev/null 2>&1; then
  fail "alpha, made before protect_from_drain, has a PodDisruptionBudget"
fi
"${FLEET_BIN}" stop "${gamma}" >/dev/null
home=$(node_of "${alpha}")
assert_equals "${home}" "$(node_of "${beta}")" "the fleet runs on one node"
share=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" persistentvolumeclaims share)
assert_equals "${class}" "$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.spec.storageClassName}')" "the share is on the movable class"
assert_equals "single-node" "$(itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.metadata.annotations.fleet\.dev/share-mode}')" "the fleet is in single-node mode"
run_in "${alpha}" 'echo on-the-share > ~/.claude/moved.txt && echo on-alpha-disk > ~/disk.txt'
run_in "${beta}" 'echo on-beta-disk > ~/disk.txt'
alpha_uid=$(uid_of "${alpha}")
beta_uid=$(uid_of "${beta}")

info "drain ${home}: the fleet moves off it together"
cordoned+=("${home}")
drain_out="${HOME}/drain.log"
itest_k8s_kubectl drain "${home}" --ignore-daemonsets --delete-emptydir-data --timeout=10m >"${drain_out}" 2>&1 &
drain_pid=$!
wait_status "node cordoned: fleet moving (${FIXTURE_REPO_NAME} off ${home}" present "the connection never said the fleet is moving" 180
wait "${drain_pid}" || fail "the drain did not finish: $(tail -n 20 "${drain_out}" | tr '\n' ' ')"
itest_k8s_wait_pod_running "$(itest_k8s_pod "$(itest_container_id "${alpha}")")" "${alpha_uid}" 300 >/dev/null
itest_k8s_wait_pod_running "$(itest_k8s_pod "$(itest_container_id "${beta}")")" "${beta_uid}" 300 >/dev/null
wait_status "fleet moving" absent "the move never ended" 300
to=$(node_of "${alpha}")
[ "${to}" != "${home}" ] || fail "alpha still runs on the drained node"
assert_equals "${to}" "$(node_of "${beta}")" "the fleet runs together on its new node"
grep -q "kube evacuate: moved the fleet.*to=${to}" "${log}" || fail "fleet.log does not record the move to ${to}"
info "$(grep -o 'kube evacuate: moved the fleet.*' "${log}" | tail -n 1 | tr -d '"')"
for ref in "${alpha}" "${beta}"; do
  assert_equals "running" "$(itest_container_status "$(itest_container_id "${ref}")")" "${ref} runs after the move"
done

info "nothing was lost: the share, each disk, one inode"
assert_equals "on-the-share" "$(run_in "${beta}" 'cat ~/.claude/moved.txt')" "the share came along"
assert_equals "on-alpha-disk" "$(run_in "${alpha}" 'cat ~/disk.txt')" "alpha's disk came along"
assert_equals "on-beta-disk" "$(run_in "${beta}" 'cat ~/disk.txt')" "beta's disk came along"
assert_equals "$(run_in "${alpha}" 'stat -c %i ~/.claude/moved.txt')" "$(run_in "${beta}" 'stat -c %i ~/.claude/moved.txt')" "one inode for both"
grep -q 'the cluster restarted an instance.*cause="fleet moved"' "${log}" \
  || fail "the moved instances' restart is not told as the fleet's move"

info "the stopped sibling starts beside them"
"${FLEET_BIN}" start "${gamma}" >/dev/null
assert_equals "${to}" "$(node_of "${gamma}")" "gamma starts on the fleet's new node"
assert_equals "on-the-share" "$(run_in "${gamma}" 'cat ~/.claude/moved.txt')" "gamma sees the share"

info "drain ${to} and kill fleetd mid-move: the next fleetd finishes the move"
itest_k8s_kubectl uncordon "${home}" >/dev/null
cordoned+=("${to}")
uids=()
for ref in "${alpha}" "${beta}" "${gamma}"; do uids+=("$(uid_of "${ref}")"); done
beta_sts=$(itest_k8s_sts "$(itest_container_id "${beta}")")
itest_k8s_kubectl drain "${to}" --ignore-daemonsets --delete-emptydir-data --timeout=10m >"${drain_out}" 2>&1 &
drain_pid=$!
# Stopped and marked: past the stop, before the start (beta's budget holds
# the fleet stopped a few seconds).
deadline=$(( $(date +%s) + $(_scale_timeout 180) ))
mark=""
while [ "$(date +%s)" -lt "${deadline}" ]; do
  mark=$(itest_k8s_kubectl get statefulset "${beta_sts}" -o 'jsonpath={.metadata.annotations.fleet\.dev/moving-off}/{.spec.replicas}' 2>/dev/null || true)
  [ "${mark}" != "${to}/0" ] || break
  sleep 0.2
done
assert_equals "${to}/0" "${mark}" "beta stopped, marked as moving off ${to}"
pkill -9 -f "${FLEET_BIN} server" || fail "no fleetd to kill"
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
while [ "$(server_count)" != "0" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "fleetd did not exit on SIGKILL"
  sleep 0.2
done
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null
wait "${drain_pid}" || fail "the drain did not finish: $(tail -n 20 "${drain_out}" | tr '\n' ' ')"
i=0
for ref in "${alpha}" "${beta}" "${gamma}"; do
  itest_k8s_wait_pod_running "$(itest_k8s_pod "$(itest_container_id "${ref}")")" "${uids[${i}]}" 300 >/dev/null
  assert_equals "${home}" "$(node_of "${ref}")" "${ref} is back on ${home}"
  i=$((i + 1))
done
grep -q 'the daemon restarted during a move' "${log}" || fail "the next fleetd did not finish the move"
deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
while [ -n "$(itest_k8s_kubectl get statefulset "${beta_sts}" -o 'jsonpath={.metadata.annotations.fleet\.dev/moving-off}')" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the move's mark outlived it"
  sleep 1
done
assert_equals "on-the-share" "$(run_in "${gamma}" 'cat ~/.claude/moved.txt')" "the share came back"
assert_equals "on-beta-disk" "$(run_in "${beta}" 'cat ~/disk.txt')" "beta's disk came back"

pass "evacuate: a drained node's single-node fleet moves together, nothing lost, finished by the next fleetd when one dies mid-move; a node-local one stays, saying why"
