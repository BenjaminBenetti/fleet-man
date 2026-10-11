#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, `fleet rebuild` is the retry of a create a fleetd kill interrupted: the instance FAILED kubernetes:interrupted (at step seed, its row offering R rebuild (retry)) is created again from its checkout, what the first try left is deleted by key first, and it reaches running at generation 1.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
UP_PID=""
itest_cleanup() {
  [ -z "${UP_PID}" ] || kill "${UP_PID}" >/dev/null 2>&1 || true
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the retry of an interrupted cluster create"

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

name=alpha
inst="${FIXTURE_REPO_NAME}/${name}"
info "fleet up ${name}, then SIGKILL to fleetd during the seed"
"${FLEET_BIN}" up "${name}" --repo "${FIXTURE_REPO_URL}" >"${TEST_SCRATCH_DIR}/up.log" 2>&1 &
UP_PID=$!
key="" seed=""
deadline=$(( $(date +%s) + $(_scale_timeout 240) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  key=$(itest_k8s_key "${name}")
  if [ -n "${key}" ]; then
    seed=$(itest_k8s_key_objects "${key}" | grep -E '^job[^/]*/.*-seed-g1$' || true)
    [ -z "${seed}" ] || break
  fi
  kill -0 "${UP_PID}" 2>/dev/null || { cat "${TEST_SCRATCH_DIR}/up.log"; fail "the create ended before its seed Job appeared"; }
  sleep 0.2
done
[ -n "${seed}" ] || fail "no seed Job appeared"
first_seed_uid=$(itest_k8s_kubectl get "${seed}" -o 'jsonpath={.metadata.uid}')
pkill -KILL -f "${FLEET_BIN} server" || fail "no fleetd to signal"
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
while [ "$(server_count)" != "0" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "fleetd did not exit on SIGKILL"
  sleep 0.2
done
wait "${UP_PID}" >/dev/null 2>&1 || true
UP_PID=""

"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
while [ "$(record_field "${name}" status)" = "creating" ] && [ "$(date +%s)" -lt "${deadline}" ]; do sleep 0.5; done
assert_equals "failed" "$(record_field "${name}" status)" "status after the interrupted create"
err=$(record_field "${name}" error)
assert_contains "${err}" "kubernetes:interrupted: " "the error's code"
assert_contains "${err}" "rebuild it to retry" "the error names the retry"
[ -z "$(record_field "${name}" container_id)" ] || fail "an interrupted create recorded a container"
[ -n "$(itest_k8s_key_objects "${key}")" ] || fail "the interrupted create left nothing to retry over"

info "fleet rebuild ${name}: the retry"
out=$("${FLEET_BIN}" rebuild "${inst}" 2>&1) || fail "the retry failed: ${out}"
assert_contains "${out}" "rebuilt" "the retry's output"
cid=$(itest_container_id "${inst}")
assert_contains "${cid}" "@g1" "the retried create's container is generation 1"
assert_container_running "${cid}" "the retried instance should be running"
assert_equals "running" "$(record_field "${name}" status)" "status after the retry"
assert_equals "" "$(record_field "${name}" error)" "the retry clears the error"
assert_equals "${key}" "$(itest_k8s_key "${name}")" "the retry keeps the instance's key"
new_seed_uid=$(itest_k8s_kubectl get "job/$(itest_k8s_sts "${cid}")-seed-g1" -o 'jsonpath={.metadata.uid}' 2>/dev/null || true)
[ "${new_seed_uid}" != "${first_seed_uid}" ] || fail "the retry kept the interrupted create's seed Job (uid ${first_seed_uid})"
assert_contains "$("${FLEET_BIN}" exec "${inst}" -- cat "/workspaces/${FIXTURE_REPO_NAME}/README.md")" "fleet" \
  "the retried instance has its workspace"
[ ! -e "${HOME}/.fleet/kube/jobs/${key}.json" ] || fail "the retry left a job journal"

info "destroy leaves no labelled object"
"${FLEET_BIN}" down "${inst}" >/dev/null
itest_k8s_wait_no_objects "${key}" 180

pass "fleet rebuild retries an interrupted cluster create: what it left deleted, created again at generation 1"
