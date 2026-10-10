#!/usr/bin/env bash
# Description: While an instance is created its row shows the current step and
# ~/.fleet/logs/<fleet>-<instance>.log (0600) fills with each step and the
# devcontainer CLI's output, containerEnv values redacted; down removes the log.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() {
  tui_kill
  [ -n "${up_pid:-}" ] && kill "${up_pid}" >/dev/null 2>&1 || true
}
itest_begin

setup_test

# The fixture, plus a containerEnv value that must reach the container but never
# the creation log: the devcontainer CLI logs its docker run with -e KEY=VALUE.
secret="itest-secret-$$-must-not-be-logged"
cat >"${FIXTURE_REPO_DIR}/.devcontainer/devcontainer.json" <<EOF
{
  "name": "fleet-man integration fixture",
  "image": "mcr.microsoft.com/devcontainers/base:debian-12",
  "containerEnv": { "FLEET_ITEST_SECRET": "${secret}" },
  "postCreateCommand": "sudo apt-get update -qq && sudo apt-get install -y -qq --no-install-recommends tmux",
  "postStartCommand": "echo poststart >> \$HOME/.fleet-poststart.log"
}
EOF
(cd "${FIXTURE_REPO_DIR}" && git commit -qam "fixture: containerEnv secret")

log_path="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-alpha.log"
up_log="${TEST_SCRATCH_DIR}/up.log"

info "fleet up alpha in the background"
"${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" >"${up_log}" 2>&1 &
up_pid=$!

tui_spawn
tui_wait_for "alpha" 30 || { cat "${up_log}"; fail "the creating instance never showed in the TUI"; }

# devcontainer up (image pull, docker run, postCreateCommand) is the long step:
# the row names it while it runs, pushed from the daemon over Watch.
info "waiting for the row to show the current step"
tui_wait_for "creating · starting container" 180 || { cat "${up_log}"; fail "the creating row never showed its step"; }

info "the creation log exists while the instance is creating"
assert_file_exists "${log_path}"
mode=$(file_mode "${log_path}")
assert_equals "600" "${mode}" "creation log mode"
grep -qF "==> starting container: devcontainer" "${log_path}" \
  || { cat "${log_path}"; fail "creation log does not show the step being run"; }

up_rc=0
wait "${up_pid}" || up_rc=$?
up_pid=""
[ "${up_rc}" -eq 0 ] || { cat "${up_log}"; cat "${log_path}"; fail "fleet up failed (rc=${up_rc})"; }

log=$(cat "${log_path}")
printf -- '--- creation log ---\n%s\n--- end ---\n' "${log}"
assert_contains "${log}" "==> fleet create: ${FIXTURE_REPO_NAME}/alpha" "creation log header"
assert_contains "${log}" "==> cloning repository" "clone step"
assert_contains "${log}" "Cloning into" "git clone output"
assert_contains "${log}" "Run: docker run" "devcontainer CLI output"
assert_contains "${log}" "FLEET_ITEST_SECRET=<redacted>" "the -e flag is logged with its value hidden"
assert_not_contains "${log}" "${secret}" "the containerEnv value leaked into the creation log"
assert_contains "${log}" "==> running startup scripts" "post-up provisioning steps"
assert_equals "==> done" "$(tail -n 1 "${log_path}")" "creation log ends with the outcome"

info "the value still reaches the container"
in_container=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- printenv FLEET_ITEST_SECRET)
assert_equals "${secret}" "${in_container}" "containerEnv inside the instance"

info "the row drops the step once the instance runs"
tui_wait_for "○ idle" 60
tui_assert_not_contains "creating ·" "a running row still shows a provisioning step"

info "fleet down removes the creation log"
"${FLEET_BIN}" down "${FIXTURE_REPO_NAME}/alpha"
assert_file_absent "${log_path}"

pass "creation log written while creating (steps, CLI output, -e redacted); row shows the step"
