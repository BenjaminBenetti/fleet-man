#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, a rebuild whose devcontainer.json (edited in the pod) has a failing postCreateCommand is rolled back: the instance runs generation 1 again with its root, workspace and edit, FAILED kubernetes:rebuild_failed with R rebuild (retry) on its row; R in the TUI opens the recovery dialog on Retry, and the retry, once the edit is fixed, rebuilds it to generation 2.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: a failed rebuild rolled back to its previous generation"

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

inst="${FIXTURE_REPO_NAME}/alpha"
in_instance() { "${FLEET_BIN}" exec "${inst}" -- "$@"; }
config() {  # <postCreateCommand>
  cat <<JSON
{
  "name": "fleet-man integration fixture (edited)",
  "image": "mcr.microsoft.com/devcontainers/base:debian-12",
  "postCreateCommand": "$1",
  "postStartCommand": "echo poststart >> \$HOME/.fleet-poststart.log"
}
JSON
}

fleet_up alpha
first=$(itest_container_id "${inst}")
assert_contains "${first}" "@g1" "a new instance is generation 1"
key=$(itest_k8s_key alpha)
in_instance sudo sh -c 'echo gen1 > /usr/local/bin/fleet-gen-marker'
itest_workspace_write "${inst}" UNCOMMITTED.txt "kept-through-rebuild"

info "edit devcontainer.json in the pod: a postCreateCommand that fails"
itest_workspace_write "${inst}" .devcontainer/devcontainer.json "$(config 'echo the edit broke the hook >&2; exit 7')"

info "fleet rebuild alpha: fails and rolls back"
rc=0
out=$("${FLEET_BIN}" rebuild "${inst}" 2>&1) || rc=$?
[ "${rc}" -ne 0 ] || fail "a rebuild whose postCreateCommand fails succeeded: ${out}"
assert_contains "${out}" "kubernetes:rebuild_failed" "the failed rebuild's code"
assert_contains "${out}" "generation 1 was restored" "the failed rebuild says what it restored"
assert_equals "failed" "$(record_field alpha status)" "status after the failed rebuild"
assert_contains "$(record_field alpha error)" "kubernetes:rebuild_failed: " "the record's error"
assert_equals "${first}" "$(itest_container_id "${inst}")" "the rolled-back instance keeps generation 1's container id"
itest_k8s_wait_pod_running "$(itest_k8s_pod "${first}")" >/dev/null
assert_container_running "${first}" "generation 1 should run again"
assert_equals "gen1" "$(in_instance cat /usr/local/bin/fleet-gen-marker)" "generation 1's root is back"
assert_equals "kept-through-rebuild" "$(itest_workspace_read "${inst}" UNCOMMITTED.txt)" "the workspace is kept"
assert_contains "$(itest_workspace_read "${inst}" .devcontainer/devcontainer.json)" "the edit broke" "the edit stays in the workspace, to fix"
assert_contains "$(cat "$(itest_host_workspace_dir "${inst}")/.devcontainer/devcontainer.json")" "the edit broke" \
  "the rebuild exported the pod's workspace to the daemon host's checkout"
[ ! -e "${HOME}/.fleet/kube/jobs/${key}.json" ] || fail "the rolled-back rebuild left its journal"
deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
while itest_k8s_key_objects "${key}" | grep -q -- '-g2$'; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "generation 2's objects outlived the rollback: $(itest_k8s_key_objects "${key}" | tr '\n' ' ')"
  sleep 2
done

info "fix the edit; R in the TUI retries"
itest_workspace_write "${inst}" .devcontainer/devcontainer.json "$(config 'echo fixed > $HOME/.fixed')"
tui_spawn
tui_wait_for "alpha" 30 || fail "the TUI never listed alpha"
tui_wait_for "rebuild failed · R rebuild (retry)" 30 || fail "the row does not offer the retry"
tui_cursor_to '.*alpha'
tui_send R
tui_wait_for "[ Retry (rebuild) ]" 10 || fail "R did not open the recovery dialog on Retry"
tui_assert_contains "> [ Retry (rebuild) ]" "the dialog opens on Retry"
tui_send Enter
deadline=$(( $(date +%s) + $(_scale_timeout 600) ))
while [ "$(record_field alpha status)" != "running" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || { tui_capture >&2; fail "the retry never finished (status $(record_field alpha status): $(record_field alpha error))"; }
  sleep 2
done
tui_kill

second=$(itest_container_id "${inst}")
assert_contains "${second}" "@g2" "the retried rebuild makes generation 2"
assert_container_absent "${first}" "generation 1 is gone after the rebuild"
assert_container_running "${second}" "generation 2 runs"
assert_equals "fixed" "$(in_instance sh -c 'cat "$HOME/.fixed"')" "the fixed postCreateCommand ran"
assert_equals "absent" "$(in_instance sh -c 'cat /usr/local/bin/fleet-gen-marker 2>/dev/null || echo absent')" "generation 2 has a fresh root"
assert_equals "kept-through-rebuild" "$(itest_workspace_read "${inst}" UNCOMMITTED.txt)" "the workspace is kept through both rebuilds"

"${FLEET_BIN}" down "${inst}" >/dev/null
itest_k8s_wait_no_objects "${key}" 180

pass "a failed rebuild is rolled back to generation 1; R retries it from the recovery dialog"
