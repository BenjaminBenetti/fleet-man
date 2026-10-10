#!/usr/bin/env bash
# Description: `fleet down` on an instance still being created cancels the create:
# the running `fleet up` fails with "instance destroyed", the record and workspace
# go, and no container is left behind once `devcontainer up` returns (before,
# the create raced the destroy and could start a container nothing recorded).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { [ -n "${up_pid:-}" ] && kill "${up_pid}" >/dev/null 2>&1 || true; }
itest_begin

setup_test
inst="${FIXTURE_REPO_NAME}/alpha"
# Every backend clones the repo on the daemon host first, so the host checkout
# marks the end of the clone step.
workspace_dir=$(itest_host_workspace_dir "${inst}")
up_log="${TEST_SCRATCH_DIR}/up.log"

info "fleet up alpha in the background"
"${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" >"${up_log}" 2>&1 &
up_pid=$!

# Interrupt it once the clone is done, i.e. while devcontainer up (or the
# provisioning after it) is working: the create is past every step that takes
# a context, so only the server's cancellation can stop it.
info "waiting for the clone to land, then interrupting the create"
cloned=""
deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  if [ -f "${workspace_dir}/.devcontainer/devcontainer.json" ]; then
    cloned=1
    break
  fi
  kill -0 "${up_pid}" 2>/dev/null || break
  sleep 0.2
done
[ -n "${cloned}" ] || { cat "${up_log}"; fail "the create never cloned its workspace"; }
grep -qF '"status": "creating"' "${HOME}/.fleet/state.json" \
  || fail "the instance is no longer creating; nothing left to interrupt"

info "fleet down alpha (mid-create)"
"${FLEET_BIN}" down "${FIXTURE_REPO_NAME}/alpha" || fail "down of a creating instance failed"

up_rc=0
wait "${up_pid}" || up_rc=$?
up_pid=""
up_out=$(cat "${up_log}")
printf '%s\n' "${up_out}"
[ "${up_rc}" -ne 0 ] || fail "fleet up succeeded although its instance was destroyed mid-create"
assert_contains "${up_out}" "instance destroyed" "fleet up should fail naming the destroy"

state=$(cat "${HOME}/.fleet/state.json")
assert_not_contains "${state}" "\"name\": \"alpha\"" "the destroyed record came back"
assert_file_absent "${workspace_dir}"
assert_workspace_absent "${inst}" "the destroyed instance's workspace is still there"

# The create job tears down the container its devcontainer up produced as soon
# as that call returns (the destroy waits for it, up to 60s).
info "checking no container is left for the destroyed workspace"
leftover=""
deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  leftover=$(itest_container_leftovers "${inst}")
  [ -z "${leftover}" ] && break
  sleep 1
done
[ -z "${leftover}" ] || fail "container(s) ${leftover} leaked for the destroyed instance"

pass "down cancels an in-flight create; nothing leaks"
