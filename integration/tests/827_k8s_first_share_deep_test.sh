#!/usr/bin/env bash
# itest: k8s
# Description: on a connection no deep test has told, the create that makes its first fleet share waits for the cluster's deep test, shown as the create's "testing the cluster" step before the share is made, and the share takes the mode the test found (its two-pod probe decides single-node shares against the copy tier); the fleet's next instance, and the next fleet's first, do not run it again.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: a cluster's deep test before its first fleet share"

setup_agent_test
other="${FIXTURE_REPO_NAME}-b"
# Two fleets with Claude Code's mount on (so each has a share), as
# seed_fleet_settings writes one.
cat > "${HOME}/.fleet/state.json" <<EOF
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {"name": "${FIXTURE_REPO_NAME}", "remote": "${FIXTURE_REPO_URL}",
      "settings": {"claudeCodeMount": true, "codexMount": false, "ghMount": false, "auggieMount": false, "buildkitServer": false, "homeDir": "/home/node"}, "instances": []},
    "${other}": {"name": "${other}", "remote": "${FIXTURE_REPO_URL}",
      "settings": {"claudeCodeMount": true, "codexMount": false, "ghMount": false, "auggieMount": false, "buildkitServer": false, "homeDir": "/home/node"}, "instances": []}
  }
}
EOF
# The share in the default class: single-node mode, also on a cluster whose
# ReadWriteMany class Auto would pick.
default_class=$(itest_k8s_kubectl get storageclass -o go-template='{{range .items}}{{if eq (index .metadata.annotations "storageclass.kubernetes.io/is-default-class") "true"}}{{.metadata.name}}{{end}}{{end}}')
[ -z "${default_class}" ] || "${FLEET_BIN}" cluster set itest shared_storage_class="${default_class}" >/dev/null

create_log() { cat "${HOME}/.fleet/logs/$1-$2.log" 2>/dev/null || true; }
share_mode() {
  local share
  share=$(itest_k8s_fleet_object "$1" persistentvolumeclaims share)
  [ -n "${share}" ] || return 0
  itest_k8s_kubectl get pvc "${share}" -o 'jsonpath={.metadata.annotations.fleet\.dev/share-mode}'
}

info "fleet up alpha: the connection's first fleet share waits for the deep test"
fleet_up alpha
log=$(create_log "${FIXTURE_REPO_NAME}" alpha)
assert_contains "${log}" "==> testing the cluster: share pods …" "alpha's create shows the deep test's two-pod probe as its step"
probe=$(printf '%s\n' "${log}" | grep -m1 -E '^==> testing the cluster: share pods: ' || true)
[ -n "${probe}" ] || fail "alpha's create shows no result of the two-pod probe:
$(printf '%s\n' "${log}" | grep -E '^==> ' | head -40)"
info "the probe: ${probe#==> testing the cluster: }"
done_line=$(printf '%s\n' "${log}" | grep -m1 -E '^==> testing the cluster: done: share mode ' || true)
[ -n "${done_line}" ] || fail "alpha's create never finished the deep test step"
mode=${done_line##* }
# The test's word decides the share: two pods on one node mounted one volume
# (a pass) is single-node mode; they could not (a fail) is the copy tier,
# with no share at all. A probe that could not tell (its pod's image not
# pulled, say) changes nothing: the share keeps the mode the preflight gave.
case "${probe}" in
  *": share pods: pass") assert_equals "single-node" "${mode}" "the share mode after a passing two-pod probe" ;;
  *": share pods: fail"*) assert_equals "copy" "${mode}" "the share mode after a failing two-pod probe" ;;
  *"could not tell"*) info "the two-pod probe could not tell; the share keeps the preflight's ${mode} mode" ;;
  *) fail "an unknown two-pod probe result: ${probe}" ;;
esac
assert_equals "$([ "${mode}" = copy ] || echo "${mode}")" "$(share_mode "${FIXTURE_REPO_NAME}")" "the fleet's share takes the deep test's mode"
# The step comes before the share is made and the image built.
first_step=$(printf '%s\n' "${log}" | grep -n -m1 -E '^==> testing the cluster: ' | cut -d: -f1)
build_step=$(printf '%s\n' "${log}" | grep -n -m1 -E '^==> (building image|applying manifests)' | cut -d: -f1)
[ -n "${build_step}" ] && [ "${first_step}" -lt "${build_step}" ] \
  || fail "the deep test step (line ${first_step}) did not come before the build (line ${build_step:-none})"

info "the fleet's next instance, and the next fleet's first, take the test's word at once"
fleet_up beta
"${FLEET_BIN}" up "${other}/alpha" >/dev/null
for ref in "${FIXTURE_REPO_NAME} beta" "${other} alpha"; do
  # shellcheck disable=SC2086
  if create_log ${ref} | grep -q '^==> testing the cluster'; then
    fail "the create of ${ref/ //} ran the deep test again"
  fi
done
assert_equals "$([ "${mode}" = copy ] || echo "${mode}")" "$(share_mode "${other}")" "the next fleet's share takes the same mode"

pass "the first fleet share on a connection waits for the deep test, shown as its step, and takes the mode it found; later creates do not run it again"
