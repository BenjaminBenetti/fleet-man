#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, a compose devcontainer is refused before anything is built or applied: the create fails kubernetes:unsupported:compose, no object carries the instance's key, and destroying it leaves nothing.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
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

"${FLEET_BIN}" down "${inst}" >/dev/null
itest_k8s_wait_no_objects "${key}" 60

pass "a compose devcontainer is refused before the build (kubernetes:unsupported:compose), nothing left"
