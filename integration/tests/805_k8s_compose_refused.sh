#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, a compose devcontainer is refused before anything is built or applied: the create fails kubernetes:unsupported:compose, no object carries the instance's key, the New instance dialog then lists the cluster unavailable for the fleet, and destroying it leaves nothing.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: compose devcontainers are not supported on a cluster"

setup_test
cat >"${FIXTURE_REPO_DIR}/.devcontainer/devcontainer.json" <<'JSON'
{
  "name": "fleet-man compose fixture",
  "dockerComposeFile": "docker-compose.yml",
  "service": "app",
  "workspaceFolder": "/workspace"
}
JSON
cat >"${FIXTURE_REPO_DIR}/.devcontainer/docker-compose.yml" <<'YAML'
services:
  app:
    image: mcr.microsoft.com/devcontainers/base:debian-12
    command: sleep infinity
YAML
(cd "${FIXTURE_REPO_DIR}" && git add -A && git commit -qm "fixture: compose")

inst="${FIXTURE_REPO_NAME}/alpha"
info "fleet up alpha (a compose devcontainer)"
rc=0
out=$("${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" 2>&1) || rc=$?
printf '%s\n' "${out}"
[ "${rc}" -ne 0 ] || fail "a compose devcontainer was created on the cluster"
assert_contains "${out}" "kubernetes:unsupported:compose" "the create's error code"

err=$(perl -MJSON::PP -e '
  open(my $fh, "<", $ARGV[0]) or exit 0;
  my $st = decode_json(do { local $/; <$fh> });
  for my $i (@{ $st->{fleets}{$ARGV[1]}{instances} || [] }) { print $i->{error} // "" if $i->{name} eq "alpha" }
' "${HOME}/.fleet/state.json" "${FIXTURE_REPO_NAME}")
assert_contains "${err}" "kubernetes:unsupported:compose" "the record's error"
key=$(itest_k8s_key "${inst}")
[ -n "${key}" ] || fail "the failed record has no key"
left=$(itest_k8s_key_objects "${key}")
assert_equals "" "${left}" "nothing is applied for a refused compose devcontainer"

log="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-alpha.log"
assert_not_contains "$(cat "${log}")" "==> building image" "a compose devcontainer is refused before the build"

# The fleet's checkout on the daemon host is a compose devcontainer, so the
# New instance dialog lists the cluster as unavailable with the reason (the
# inspector's check/kubernetes) instead of offering it.
cluster=$(itest_kube_clusters 'print $c[0]{display_name} || $c[0]{id}')
tui_spawn
tui_wait_for "alpha" 15 || fail "the TUI did not show the fleet"
info "pressing 'a' on the fleet header"
tui_send a
tui_wait_for "instance-name" 5 || fail "the New instance dialog did not open"
# The dialog box wraps the reason; read it with the box's borders taken out.
dialog=$(tui_capture | perl -CS -Mutf8 -pe 's/[│╭╮╰╯─]//g' | tr -s ' \n' '  ')
assert_contains "${dialog}" "☸ ${cluster} ✕ compose devcontainers are not supported on Kubernetes" "the Deploy row should list the cluster unavailable for a compose fleet"
tui_send Escape
sleep 0.3
tui_send q
tui_wait_for_absent "instance-name" 5 || fail "the New instance dialog did not close"
tui_kill

"${FLEET_BIN}" down "${inst}" >/dev/null
itest_k8s_wait_no_objects "${key}" 60

pass "a compose devcontainer is refused before the build (kubernetes:unsupported:compose) and listed unavailable on the Deploy row, nothing left"
