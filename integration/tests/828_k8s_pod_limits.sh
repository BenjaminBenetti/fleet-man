#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, an instance pod's dev container gets the connection's 8Gi memory limit by default and no CPU limit (pod-init holding the user cgroup just under it where it lays one out); memory_limit=0 and resources.cpu_limit=1 give the next instance no memory limit and a 1-CPU limit while the first keeps its own; a LimitRange maximum below memory_limit clamps the next one's limit to it, with a warning; `fleet cluster set` takes 0 for no limit and refuses another zero.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { itest_k8s_kubectl delete limitrange fleet-itest-pod-limits --ignore-not-found >/dev/null 2>&1 || true; }
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: instance pod limits"

setup_test
# Small requests, so three instances fit one node.
out=$("${FLEET_BIN}" cluster set itest resources.cpu_request=100m resources.memory_request=256Mi 2>&1) \
  || fail "fleet cluster set itest: ${out}"

# limits <instance> — the dev container's limits, as "cpu=<q> memory=<q>".
limits() {
  local pod
  pod=$(itest_k8s_pod "$(itest_container_id "${FIXTURE_REPO_NAME}/$1")")
  itest_k8s_kubectl get pod "${pod}" -o \
    'jsonpath=cpu={.spec.containers[?(@.name=="dev")].resources.limits.cpu} memory={.spec.containers[?(@.name=="dev")].resources.limits.memory}'
}

info "fleet up alpha: memory_limit's 8Gi, no CPU limit"
out=$(fleet_up alpha 2>&1) || fail "fleet up alpha failed: ${out}"
assert_equals "cpu= memory=8Gi" "$(limits alpha)" "alpha's limits"
# Where pod-init lays out cgroups (a privileged pod), the user's processes
# sit 256Mi under the 8Gi.
layout=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /opt/fleet/run/cgroup.json 2>/dev/null || true)
if [ -n "${layout}" ]; then
  assert_contains "${layout}" "\"user_memory_max\":$(( (8 << 30) - (256 << 20) ))" "fleet-user's memory.max under the 8Gi"
else
  info "no cgroup layout (an unprivileged pod): the container's limit alone holds"
fi

info "memory_limit=0 and resources.cpu_limit=1: beta has a CPU limit and no memory limit"
out=$("${FLEET_BIN}" cluster set itest memory_limit=0 resources.cpu_limit=1 2>&1) || fail "fleet cluster set itest (limits): ${out}"
out=$(fleet_up beta 2>&1) || fail "fleet up beta failed: ${out}"
assert_equals "cpu=1 memory=" "$(limits beta)" "beta's limits"
assert_equals "cpu= memory=8Gi" "$(limits alpha)" "alpha keeps its limits until it is rebuilt"

info "a LimitRange maximum of 4Gi clamps gamma's limit to it"
out=$("${FLEET_BIN}" cluster set itest memory_limit= resources.cpu_limit= 2>&1) || fail "fleet cluster set itest (defaults): ${out}"
itest_k8s_kubectl apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: LimitRange
metadata:
  name: fleet-itest-pod-limits
spec:
  limits:
  - type: Container
    max:
      memory: 4Gi
EOF
test_out=$("${FLEET_BIN}" cluster test itest 2>&1) || true
assert_contains "${test_out}" "LimitRange maximum memory 4Gi → memory limit clamped from 8Gi" "the limits check should say it clamps"
out=$(fleet_up gamma 2>&1) || fail "fleet up gamma failed: ${out}"
assert_equals "cpu= memory=4Gi" "$(limits gamma)" "gamma's clamped limit"
warn_file="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-gamma.warn"
[ -f "${warn_file}" ] || fail "no warning banner for gamma"
assert_contains "$(cat "${warn_file}")" "LimitRange: maximum memory 4Gi → memory limit clamped from 8Gi" "gamma's banner names the clamp"

info "0 is the only zero"
if out=$("${FLEET_BIN}" cluster set itest memory_limit=0Gi 2>&1); then
  fail "memory_limit=0Gi was taken: ${out}"
fi
assert_contains "${out}" "memory_limit must be more than zero (or 0 for no limit)" "the refusal says how to turn it off"

pass "instance pods take the connection's limits"
