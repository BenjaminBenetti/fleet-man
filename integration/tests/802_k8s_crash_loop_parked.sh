#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, with no TUI connected, a dev container that keeps exiting is parked as stopped (kubernetes:crash_loop on its record, `fleet ls` stopped), its runs' output is kept so `fleet logs` shows the crashed runs, and a start brings it back.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: crash-loop parking"

setup_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"
cid=$(itest_container_id "${inst}")
pod=$(itest_k8s_pod "${cid}")
itest_k8s_wait_pod_running "${pod}" >/dev/null

record_field() {
  perl -MJSON::PP -e '
    my ($path, $fleet, $inst, $field) = @ARGV;
    open(my $fh, "<", $path) or exit 0;
    my $st = eval { local $/; decode_json(<$fh>) } or exit 0;
    for my $i (@{ $st->{fleets}{$fleet}{instances} || [] }) {
      print $i->{$field} // "" if $i->{name} eq $inst;
    }
  ' "${HOME}/.fleet/state.json" "${FIXTURE_REPO_NAME}" alpha "$1"
}

restarts() {
  itest_k8s_kubectl get pod "${pod}" -o 'jsonpath={.status.containerStatuses[?(@.name=="dev")].restartCount}' 2>/dev/null || echo 0
}

# Each run says which it is on PID 1's output (the container log), then the
# kubelet's own stop signal ends it, so the container exits and is restarted.
# No TUI is connected: fleetd parks the loop from its pod events alone.
info "crashing the dev container until fleetd parks it"
run=0
deadline=$(( $(date +%s) + $(_scale_timeout 420) ))
while [ "$(record_field status)" = "running" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "fleetd never parked the crash loop (restarts: $(restarts))"
  phase=$(itest_k8s_kubectl get pod "${pod}" -o 'jsonpath={.status.containerStatuses[?(@.name=="dev")].state.running.startedAt}' 2>/dev/null || true)
  if [ -n "${phase}" ]; then
    run=$((run + 1))
    info "crashing run ${run} (restarts so far: $(restarts))"
    itest_k8s_kubectl exec "${pod}" -c dev -- sh -c "echo itest-crashed-run-${run} >/proc/1/fd/1; kill -TERM 1" >/dev/null 2>&1 || true
    # Wait for that run to end before looking again.
    for _ in $(seq 1 60); do
      [ -z "$(itest_k8s_kubectl get pod "${pod}" -o 'jsonpath={.status.containerStatuses[?(@.name=="dev")].state.running.startedAt}' 2>/dev/null || true)" ] && break
      [ "$(itest_k8s_kubectl get pod "${pod}" -o 'jsonpath={.status.containerStatuses[?(@.name=="dev")].state.running.startedAt}' 2>/dev/null || true)" != "${phase}" ] && break
      sleep 1
    done
  fi
  sleep 1
done

info "parked after ${run} crashed run(s)"
assert_equals "stopped" "$(record_field status)" "the parked record"
assert_contains "$(record_field error)" "kubernetes:crash_loop: " "the parked record says why"
assert_contains "$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")" "stopped" "fleet ls shows it stopped"
assert_container_stopped "${cid}" "the parked StatefulSet is scaled to 0"

info "fleet logs shows the crashed runs"
logs=$("${FLEET_BIN}" logs "${inst}" 2>&1)
printf '%s\n' "${logs}" | tail -n 30
[ "${run}" -ge 1 ] || fail "no run was crashed"
for n in $(seq 1 "${run}"); do
  assert_contains "${logs}" "itest-crashed-run-${n}" "fleet logs keeps crashed run ${n}"
done

info "a start brings it back"
"${FLEET_BIN}" start "${inst}" >/dev/null
assert_container_running "${cid}" "the instance runs again"
assert_equals "back" "$("${FLEET_BIN}" exec "${inst}" -- echo back)" "exec after the start"
assert_equals "" "$(record_field error)" "the start clears the parked error"

pass "a crash loop is parked without a TUI, its runs kept for fleet logs, and starts again"
