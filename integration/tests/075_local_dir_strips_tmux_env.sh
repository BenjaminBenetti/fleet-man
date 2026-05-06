#!/usr/bin/env bash
# Description: local_dir exec scrubs $TMUX/$TMUX_PANE so a nested tmux can start cleanly.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
"${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" --backend local_dir

# Simulate being launched from inside an outer tmux by setting TMUX/
# TMUX_PANE in the env we pass to fleet. local_dir's exec must strip
# them so a nested tmux invocation does not error with "sessions
# should be nested with care".
info "fleet exec sees \$TMUX scrubbed"
seen_tmux=$(TMUX="/tmp/fake-tmux,1234,0" TMUX_PANE="%99" \
  "${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c 'printf "%s" "${TMUX:-EMPTY}"')
assert_equals "EMPTY" "${seen_tmux}" "TMUX should be unset inside local_dir exec"

seen_pane=$(TMUX="/tmp/fake-tmux,1234,0" TMUX_PANE="%99" \
  "${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c 'printf "%s" "${TMUX_PANE:-EMPTY}"')
assert_equals "EMPTY" "${seen_pane}" "TMUX_PANE should be unset inside local_dir exec"

# Sanity: other env vars still pass through (PATH must be present or
# almost no command would work).
seen_path=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c 'printf "%s" "${PATH:-MISSING}"')
if [ "${seen_path}" = "MISSING" ] || [ -z "${seen_path}" ]; then
  fail "PATH should pass through to local_dir exec, got: [${seen_path}]"
fi

# End-to-end: a real nested tmux command (the failure mode the user
# hit) must succeed when invoked through local_dir exec from inside
# a fake outer-tmux env. We start a detached session, list it, then
# kill it. Skip if tmux isn't installed on the runner.
if command -v tmux >/dev/null 2>&1; then
  info "nested tmux start under fake outer TMUX"
  socket_dir="$(mktemp -d)"
  TMUX="/tmp/fake-tmux,1234,0" TMUX_PANE="%99" \
    "${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- \
    tmux -S "${socket_dir}/sock" new-session -d -s nested-itest 'sleep 5'
  list_out=$(tmux -S "${socket_dir}/sock" list-sessions -F "#{session_name}" 2>&1 || true)
  tmux -S "${socket_dir}/sock" kill-server >/dev/null 2>&1 || true
  rm -rf "${socket_dir}"
  assert_contains "${list_out}" "nested-itest" "nested tmux session should be listed"
fi

pass "strips TMUX env (local_dir)"
