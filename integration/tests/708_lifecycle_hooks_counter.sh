#!/usr/bin/env bash
# Description: devcontainer lifecycle hooks run the right number of times, in spec order: create runs initialize/onCreate/updateContent/postCreate/postStart/postAttach once each, start re-runs only postStart/postAttach, and rebuild runs the full set again.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-lifecycle" setup_test

inst="${FIXTURE_REPO_NAME}/alpha"

# The fixture's hooks append their names to .lifecycle.log in the workspace,
# which survives stop/start and rebuild, so the log is the hooks' full history.
# initializeCommand runs on the daemon host (every backend builds there), so it
# logs beside the host checkout instead.
hook_log() { itest_workspace_read "${inst}" .lifecycle.log 2>/dev/null | tr '\n' ' ' | sed 's/ $//'; }
init_count() {
  local f
  f="$(dirname "$(itest_host_workspace_dir "${inst}")")/initialize.log"
  if [ -f "${f}" ]; then grep -c '^initialize$' "${f}"; else echo 0; fi
}

create_hooks="onCreate updateContent postCreate postStart postAttach"
start_hooks="postStart postAttach"

info "fleet up alpha"
fleet_up alpha
assert_equals "${create_hooks}" "$(hook_log)" "create should run every in-container hook once, in spec order"
assert_equals "1" "$(init_count)" "create should run initializeCommand once on the daemon host"

info "fleet stop + start alpha"
"${FLEET_BIN}" stop "${inst}" >/dev/null
assert_equals "${create_hooks}" "$(hook_log)" "stop must not run any hook"
"${FLEET_BIN}" start "${inst}" >/dev/null
assert_equals "${create_hooks} ${start_hooks}" "$(hook_log)" \
  "start should re-run only postStart and postAttach"
assert_equals "1" "$(init_count)" "start must not re-run initializeCommand"

info "fleet start alpha again (already running)"
"${FLEET_BIN}" start "${inst}" >/dev/null
assert_equals "${create_hooks} ${start_hooks}" "$(hook_log)" "an already-running start must not run any hook"

info "fleet rebuild alpha"
"${FLEET_BIN}" rebuild "${inst}" >/dev/null
assert_equals "${create_hooks} ${start_hooks} ${create_hooks}" "$(hook_log)" \
  "rebuild should run the full create-time set again"
assert_equals "2" "$(init_count)" "rebuild should run initializeCommand again"

info "fleet stop + start alpha after the rebuild"
"${FLEET_BIN}" stop "${inst}" >/dev/null
"${FLEET_BIN}" start "${inst}" >/dev/null
assert_equals "${create_hooks} ${start_hooks} ${create_hooks} ${start_hooks}" "$(hook_log)" \
  "a start after a rebuild should again re-run only postStart and postAttach"
assert_equals "2" "$(init_count)" "start must not re-run initializeCommand"

pass "lifecycle hooks: once per create, postStart/postAttach once per start, full set on rebuild"
