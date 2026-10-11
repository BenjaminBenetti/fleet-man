#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, cloning a privileged (freezable) instance freezes the source's user processes for the final copy while its agent stays live; a fleetd killed (SIGKILL) during the freeze, its pod agent killed too, leaves the source frozen no longer than the freeze's watchdog (pod-init, PID 1, thaws it), the source runs on, the next fleetd fails the clone kubernetes:interrupted and deletes its copier Job, and destroying the clone leaves no labelled object.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
CLONE_PID=""
itest_cleanup() {
  [ -z "${CLONE_PID}" ] || kill "${CLONE_PID}" >/dev/null 2>&1 || true
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the freeze is a cluster pod's user cgroup"

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-privileged" setup_test

# record_field <instance> <field> — a field of the instance's record.
record_field() {
  perl -MJSON::PP -e '
    my ($path, $fleet, $inst, $field) = @ARGV;
    open(my $fh, "<", $path) or exit 0;
    my $st = eval { local $/; decode_json(<$fh>) } or exit 0;
    for my $i (@{ $st->{fleets}{$fleet}{instances} || [] }) {
      print $i->{$field} // "" if $i->{name} eq $inst;
    }
  ' "${HOME}/.fleet/state.json" "${FIXTURE_REPO_NAME}" "$1" "$2"
}

info "fleet up alpha (privileged)"
fleet_up alpha >/dev/null
src="${FIXTURE_REPO_NAME}/alpha"
src_container=$(itest_container_id "${src}")
pod=$(itest_k8s_pod "${src_container}")

# kubectl exec lands in PID 1's cgroup (fleet-agent), which is never frozen:
# it can look at the user cgroup while it is.
in_pod() { itest_k8s_kubectl exec "${pod}" -c dev -- "$@"; }
user_cg=$(in_pod cat /opt/fleet/run/cgroup.json | perl -MJSON::PP -e 'my $l = eval { decode_json(join "", <STDIN>) } or exit 0; print $l->{user} // ""')
[ -n "${user_cg}" ] || fail "the privileged pod has no cgroup layout (/opt/fleet/run/cgroup.json)"
info "alpha's user cgroup: ${user_cg}"
freeze_state() { in_pod cat "${user_cg}/cgroup.freeze" 2>/dev/null | tr -d '[:space:]'; }

# A ticker the user runs: fleet exec starts it in the user cgroup.
"${FLEET_BIN}" exec "${src}" -- sh -c 'setsid sh -c "while :; do date +%s%N > /tmp/itest-tick; sleep 0.2; done" >/dev/null 2>&1 </dev/null &'
tick() { in_pod cat /tmp/itest-tick 2>/dev/null || true; }
t0=$(tick); sleep 1
[ "$(tick)" != "${t0}" ] || fail "the ticker in alpha does not tick"
ticker_pid=$(in_pod sh -c 'pgrep -f "itest-[t]ick" | head -n 1')
[ -n "${ticker_pid}" ] || fail "no ticker process in alpha's pod"
in_pod grep -q "fleet-user" "/proc/${ticker_pid}/cgroup" \
  || fail "a command fleet exec started is not in the user cgroup: $(in_pod cat "/proc/${ticker_pid}/cgroup")"

# A fleetd that holds the frozen source two minutes, far past the freeze's
# 30s watchdog, so it is killed while the source is frozen.
info "restarting fleetd to hold a clone's frozen source (FLEET_KUBE_CLONE_HOLD_PAUSE)"
pkill -TERM -f "${FLEET_BIN} server" || true
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
while [ "$(server_count)" != "0" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "fleetd did not stop"
  sleep 0.2
done
FLEET_KUBE_CLONE_HOLD_PAUSE=120s "${FLEET_BIN}" clone "${src}" beta >"${TEST_SCRATCH_DIR}/clone.log" 2>&1 &
CLONE_PID=$!

info "waiting for the clone to freeze alpha for its final copy"
deadline=$(( $(date +%s) + $(_scale_timeout 300) ))
until [ "$(freeze_state)" = "1" ]; do
  kill -0 "${CLONE_PID}" 2>/dev/null || { cat "${TEST_SCRATCH_DIR}/clone.log"; fail "the clone ended before it froze alpha"; }
  [ "$(date +%s)" -lt "${deadline}" ] || { cat "${TEST_SCRATCH_DIR}/clone.log"; fail "the clone never froze alpha"; }
  sleep 0.5
done
frozen_at=$(date +%s)
sleep 2
t1=$(tick); sleep 2
assert_equals "${t1}" "$(tick)" "alpha's ticker must stop while its user cgroup is frozen"
# The agent, and an exec the kubelet starts, still run.
in_pod true || fail "a kubectl exec stalled during the freeze"

info "killing fleetd and alpha's pod agent during the freeze"
pkill -KILL -f "${FLEET_BIN} server" || fail "no fleetd to kill"
in_pod sh -c 'pkill -KILL -f "[p]od-agent serve" || true'
wait "${CLONE_PID}" >/dev/null 2>&1 || true
CLONE_PID=""

info "waiting for pod-init's watchdog to thaw alpha"
deadline=$(( frozen_at + $(_scale_timeout 75) ))
until [ "$(freeze_state)" = "0" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "alpha is still frozen $(( $(date +%s) - frozen_at ))s after the freeze"
  sleep 1
done
thawed=$(( $(date +%s) - frozen_at ))
info "thawed ${thawed}s after the freeze"
[ "${thawed}" -ge 20 ] || fail "alpha thawed after ${thawed}s: something but the watchdog thawed it"
t2=$(tick); sleep 1
[ "$(tick)" != "${t2}" ] || fail "alpha's ticker did not resume after the thaw"

info "the next fleetd fails the interrupted clone and deletes its copier"
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null
status="" err=""
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  status=$(record_field beta status)
  [ "${status}" = "cloning" ] || break
  sleep 0.5
done
err=$(record_field beta error)
assert_equals "failed" "${status}" "beta's status after the interrupted clone"
assert_contains "${err}" "kubernetes:interrupted: " "beta's error code"
assert_contains "${err}" "during clone" "beta's error names the clone"
key=$(itest_k8s_key beta)
[ -n "${key}" ] || fail "beta has no kube key"
deadline=$(( $(date +%s) + $(_scale_timeout 240) ))
while itest_k8s_key_objects "${key}" | grep -q '^job'; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the interrupted clone's copier Job is still there: $(itest_k8s_key_objects "${key}" | tr '\n' ' ')"
  sleep 3
done

assert_container_running "${src_container}" "the source must run on after the interrupted clone"
"${FLEET_BIN}" exec "${src}" -- true || fail "alpha does not answer fleet exec after the interrupted clone"

info "destroy beta: nothing of its key is left"
"${FLEET_BIN}" down "${FIXTURE_REPO_NAME}/beta" >/dev/null
itest_k8s_wait_no_objects "${key}" 180

pass "a fleetd killed mid-freeze leaves the source frozen no longer than its watchdog; the clone fails interrupted and its copier goes"
