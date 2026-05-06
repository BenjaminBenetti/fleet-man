#!/usr/bin/env bash
# Description: TUI session list for a local_dir instance does not leak unrelated host tmux sessions.
set -euo pipefail

source "$(dirname "$0")/../common.sh"

HOST_SESSION="itest-host-leak-$$"

itest_cleanup() {
  tmux kill-session -t "${HOST_SESSION}" >/dev/null 2>&1 || true
  tui_kill
}
itest_begin

setup_test
"${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" --backend local_dir

# Spawn a host-side tmux session that does NOT match the instance's
# session-name prefix. local_dir shares the host tmux server, so this
# session would be visible to the instance unless the discovery code
# filters by prefix.
tmux new-session -d -s "${HOST_SESSION}" 'sleep 60'

# Sanity: the host session exists.
list=$(tmux list-sessions -F "#{session_name}" 2>/dev/null || true)
assert_contains "${list}" "${HOST_SESSION}" "host tmux session should have been created"

info "spawning fleet TUI"
tui_spawn

info "waiting for TUI to hydrate"
tui_wait_for "alpha" 15

info "moving cursor to alpha row and expanding"
tui_send j
sleep 0.5
tui_send Space
tui_wait_for "+ new session" 15

# Wait for the session-discovery loop to tick at least once (1s cadence).
sleep 2

screen=$(tui_capture)
printf -- '--- screen ---\n%s\n--- end ---\n' "${screen}"

# The unrelated host session must NOT appear anywhere in the rendered
# session list.
if printf '%s' "${screen}" | grep -qF -- "${HOST_SESSION}"; then
  fail "host tmux session ${HOST_SESSION} leaked into local_dir instance's session list"
fi

# The fleet TUI itself runs inside the outer tmux session named
# "fleetman-itest" (set by tui_spawn). It must also be filtered out.
if printf '%s' "${screen}" | grep -qF -- "fleetman-itest"; then
  fail "outer fleet TUI tmux session 'fleetman-itest' leaked into instance's session list"
fi

pass "host tmux sessions filtered (local_dir)"
