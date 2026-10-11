#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, a fleetd killed (SIGKILL) or stopped (SIGTERM, its bounded shutdown) while it seeds a new instance's disk leaves the instance FAILED kubernetes:interrupted (at step seed) once the next fleetd starts, never resumed; destroying it leaves no labelled object.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
UP_PID=""
itest_cleanup() {
  [ -z "${UP_PID}" ] || kill "${UP_PID}" >/dev/null 2>&1 || true
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the create journal and its reconciler"

setup_test

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

# interrupt <instance> <signal> — start creating <instance>, and send fleetd
# <signal> once its seed Job exists; then start a new fleetd and check what
# its reconciler made of the create, and that destroy cleans up.
interrupt() {
  local name="$1" sig="$2" key="" seed="" deadline
  info "${name}: fleet up, then SIG${sig} to fleetd during the seed"
  "${FLEET_BIN}" up "${name}" --repo "${FIXTURE_REPO_URL}" >"${TEST_SCRATCH_DIR}/up-${name}.log" 2>&1 &
  UP_PID=$!
  deadline=$(( $(date +%s) + $(_scale_timeout 240) ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    key=$(itest_k8s_key "${name}")
    if [ -n "${key}" ]; then
      seed=$(itest_k8s_key_objects "${key}" | grep -E '^job[^/]*/.*-seed-g1$' || true)
      [ -z "${seed}" ] || break
    fi
    kill -0 "${UP_PID}" 2>/dev/null || { cat "${TEST_SCRATCH_DIR}/up-${name}.log"; fail "${name}: the create ended before its seed Job appeared"; }
    sleep 0.2
  done
  [ -n "${seed}" ] || fail "${name}: no seed Job appeared"
  info "${name}: ${seed} exists; SIG${sig} to fleetd"
  pkill "-${sig}" -f "${FLEET_BIN} server" || fail "no fleetd to signal"
  deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
  while [ "$(server_count)" != "0" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "${name}: fleetd did not exit on SIG${sig}"
    sleep 0.2
  done
  wait "${UP_PID}" >/dev/null 2>&1 || true
  UP_PID=""

  info "${name}: the next fleetd fails the interrupted create"
  "${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null
  local status="" err=""
  deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    status=$(record_field "${name}" status)
    [ "${status}" = "creating" ] || break
    sleep 0.5
  done
  err=$(record_field "${name}" error)
  assert_equals "failed" "${status}" "${name}: status after the interrupted create"
  assert_contains "${err}" "kubernetes:interrupted: " "${name}: the error's code"
  assert_contains "${err}" "(at step seed)" "${name}: the error names the step"
  assert_contains "$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")" "failed" "${name}: fleet ls shows it failed"
  [ ! -e "${HOME}/.fleet/kube/jobs/${key:0:8}.json" ] || info "${name}: the create's journal is still there"

  # Never resumed: it stays failed.
  sleep 3
  assert_equals "failed" "$(record_field "${name}" status)" "${name}: an interrupted create is not resumed"

  info "${name}: destroy leaves no labelled object"
  "${FLEET_BIN}" down "${FIXTURE_REPO_NAME}/${name}" >/dev/null
  itest_k8s_wait_no_objects "${key}" 180
}

interrupt killed KILL
interrupt stopped TERM

pass "a fleetd killed or stopped mid-seed leaves FAILED kubernetes:interrupted (at step seed); destroy leaves nothing"
