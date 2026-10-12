#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, a fleetd killed (SIGKILL) or stopped (SIGTERM, its bounded shutdown leaves the rebuild at its journal) once a rebuild has reset the instance's root is reconciled by the next fleetd: the rollback Job puts generation 1's root back, its StatefulSet and replicas are restored, the record goes FAILED kubernetes:rebuild_failed naming the restored generation, and a rebuild then succeeds. One killed before the rebuild stopped the instance (during its export, build or delivery) leaves it untouched: the next fleetd sets the record back to running, with no error, and a rebuild then succeeds.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
RB_PID=""
itest_cleanup() {
  [ -z "${RB_PID}" ] || kill "${RB_PID}" >/dev/null 2>&1 || true
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the rebuild journal and its reconciler"

setup_test

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

# journal_reset <key> — whether the instance's job journal says its rebuild
# has reached its reset.
journal_reset() {
  perl -MJSON::PP -e '
    open(my $fh, "<", $ARGV[0]) or exit 1;
    my $j = eval { local $/; decode_json(<$fh>) } or exit 1;
    exit($j->{resetStarted} ? 0 : 1);
  ' "${HOME}/.fleet/kube/jobs/$1.json"
}

# journal_step <key> — the step the instance's job journal records.
journal_step() {
  perl -MJSON::PP -e '
    open(my $fh, "<", $ARGV[0]) or exit 1;
    my $j = eval { local $/; decode_json(<$fh>) } or exit 1;
    print $j->{step} // "";
  ' "${HOME}/.fleet/kube/jobs/$1.json" 2>/dev/null || true
}

# interrupt_early <instance> — SIGKILL fleetd while a rebuild exports, builds
# or delivers: before it stopped the instance.
interrupt_early() {
  local name="$1" inst key first deadline step
  inst="${FIXTURE_REPO_NAME}/${name}"
  fleet_up "${name}"
  first=$(itest_container_id "${inst}")
  key=$(itest_k8s_key "${name}")
  "${FLEET_BIN}" exec "${inst}" -- sudo sh -c 'echo gen1 > /usr/local/bin/fleet-gen-marker'

  info "${name}: fleet rebuild, then SIGKILL to fleetd before the rebuild stops the instance"
  "${FLEET_BIN}" rebuild "${inst}" >"${TEST_SCRATCH_DIR}/rebuild-${name}.log" 2>&1 &
  RB_PID=$!
  deadline=$(( $(date +%s) + $(_scale_timeout 600) ))
  while :; do
    step=$(journal_step "${key}")
    case "${step}" in export|build|deliver) break ;; esac
    [ "$(date +%s)" -lt "${deadline}" ] || fail "${name}: the rebuild's journal never named its export, build or delivery (last: ${step})"
    kill -0 "${RB_PID}" 2>/dev/null || { cat "${TEST_SCRATCH_DIR}/rebuild-${name}.log"; fail "${name}: the rebuild ended before it was killed"; }
    sleep 0.1
  done
  pkill -KILL -f "${FLEET_BIN} server" || fail "no fleetd to signal"
  deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
  while [ "$(server_count)" != "0" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "${name}: fleetd did not exit on SIGKILL"
    sleep 0.2
  done
  wait "${RB_PID}" >/dev/null 2>&1 || true
  RB_PID=""
  info "${name}: killed at step ${step}"
  assert_equals "rebuilding" "$(record_field "${name}" status)" "${name}: the record the killed rebuild left"

  info "${name}: the next fleetd sets the record back to running"
  "${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null
  deadline=$(( $(date +%s) + $(_scale_timeout 600) ))
  while [ "$(record_field "${name}" status)" = "rebuilding" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "${name}: the interrupted rebuild was never settled"
    sleep 2
  done
  assert_equals "running" "$(record_field "${name}" status)" "${name}: status after a rebuild killed before it changed anything"
  assert_equals "" "$(record_field "${name}" error)" "${name}: no error on the record"
  assert_equals "${first}" "$(itest_container_id "${inst}")" "${name}: the record keeps generation 1's container id"
  assert_equals "gen1" "$("${FLEET_BIN}" exec "${inst}" -- cat /usr/local/bin/fleet-gen-marker)" "${name}: generation 1's root is untouched"
  [ ! -e "${HOME}/.fleet/kube/jobs/${key}.json" ] || fail "${name}: the settled rebuild's journal is still there"

  info "${name}: fleet rebuild again succeeds"
  "${FLEET_BIN}" rebuild "${inst}" >/dev/null || fail "${name}: the rebuild after the interrupted one failed"
  assert_contains "$(itest_container_id "${inst}")" "@g2" "${name}: the rebuild makes generation 2"

  "${FLEET_BIN}" down "${inst}" >/dev/null
  itest_k8s_wait_no_objects "${key}" 180
}

# interrupt <instance> <signal>
interrupt() {
  local name="$1" sig="$2" inst key first deadline
  inst="${FIXTURE_REPO_NAME}/${name}"
  fleet_up "${name}"
  first=$(itest_container_id "${inst}")
  key=$(itest_k8s_key "${name}")
  "${FLEET_BIN}" exec "${inst}" -- sudo sh -c 'echo gen1 > /usr/local/bin/fleet-gen-marker'
  itest_workspace_write "${inst}" UNCOMMITTED.txt "kept"

  info "${name}: fleet rebuild, then SIG${sig} to fleetd once the root is reset"
  "${FLEET_BIN}" rebuild "${inst}" >"${TEST_SCRATCH_DIR}/rebuild-${name}.log" 2>&1 &
  RB_PID=$!
  deadline=$(( $(date +%s) + $(_scale_timeout 600) ))
  until journal_reset "${key}"; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "${name}: the rebuild never reached its reset"
    kill -0 "${RB_PID}" 2>/dev/null || { cat "${TEST_SCRATCH_DIR}/rebuild-${name}.log"; fail "${name}: the rebuild ended before its reset"; }
    sleep 0.2
  done
  pkill "-${sig}" -f "${FLEET_BIN} server" || fail "no fleetd to signal"
  deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
  while [ "$(server_count)" != "0" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "${name}: fleetd did not exit on SIG${sig}"
    sleep 0.2
  done
  wait "${RB_PID}" >/dev/null 2>&1 || true
  RB_PID=""
  assert_equals "rebuilding" "$(record_field "${name}" status)" "${name}: the record the killed rebuild left"
  [ -e "${HOME}/.fleet/kube/jobs/${key}.json" ] || fail "${name}: the interrupted rebuild left no journal"

  info "${name}: the next fleetd rolls the rebuild back"
  "${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null
  deadline=$(( $(date +%s) + $(_scale_timeout 600) ))
  while [ "$(record_field "${name}" status)" = "rebuilding" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "${name}: the interrupted rebuild was never undone"
    sleep 2
  done
  assert_equals "failed" "$(record_field "${name}" status)" "${name}: status after the rollback"
  local err
  err=$(record_field "${name}" error)
  assert_contains "${err}" "kubernetes:rebuild_failed: " "${name}: the error's code"
  assert_contains "${err}" "generation 1 was restored" "${name}: the error names the restored generation"
  assert_equals "${first}" "$(itest_container_id "${inst}")" "${name}: the record keeps generation 1's container id"
  itest_k8s_wait_pod_running "$(itest_k8s_pod "${first}")" >/dev/null
  assert_container_running "${first}" "${name}: generation 1 runs again"
  assert_equals "gen1" "$("${FLEET_BIN}" exec "${inst}" -- cat /usr/local/bin/fleet-gen-marker)" "${name}: generation 1's root is back"
  assert_equals "kept" "$(itest_workspace_read "${inst}" UNCOMMITTED.txt)" "${name}: the workspace is kept"
  [ ! -e "${HOME}/.fleet/kube/jobs/${key}.json" ] || fail "${name}: the rolled-back rebuild's journal is still there"

  info "${name}: fleet rebuild again succeeds"
  "${FLEET_BIN}" rebuild "${inst}" >/dev/null || fail "${name}: the rebuild after the rollback failed"
  assert_contains "$(itest_container_id "${inst}")" "@g2" "${name}: the rebuild makes generation 2"
  assert_equals "running" "$(record_field "${name}" status)" "${name}: status after the rebuild"
  assert_equals "absent" "$("${FLEET_BIN}" exec "${inst}" -- sh -c 'cat /usr/local/bin/fleet-gen-marker 2>/dev/null || echo absent')" "${name}: a fresh root"

  "${FLEET_BIN}" down "${inst}" >/dev/null
  itest_k8s_wait_no_objects "${key}" 180
}

interrupt killed KILL
interrupt stopped TERM
interrupt_early early

pass "a fleetd killed or stopped mid-rebuild is reconciled: generation 1 restored, FAILED kubernetes:rebuild_failed, then rebuilt; killed before the rebuild changed anything, running as it was"
