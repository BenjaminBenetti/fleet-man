#!/usr/bin/env bash
# Description: TUI session list resolves the devcontainer remoteUser even when a second container user holds a tmux socket (containerUser regression)
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

# The two-user fixture provisions an `app` user (uid 4001, /home/app) next to the
# vscode remoteUser. tmux server sockets are per-uid (/tmp/tmux-<uid>/), so the
# server's session poll (docker exec -u $(containerUser)) and the session-create
# path (devcontainer exec, runs as the vscode remoteUser) MUST agree on the user
# or created sessions land on a socket the poll never reads.
#
# The old containerUser() guessed: first /tmp/tmux-[1-9]*/ owner, else first
# /home/<user> owner. With /home/app sorting before /home/vscode — and app also
# holding its own tmux socket below — that guess latches onto `app` and is cached
# forever, so the (vscode) session fleet creates would never appear in the list.
# The fix resolves the real remoteUser (vscode) from the devcontainer.metadata
# label, so neither sort order nor socket timing can mislead it. This test fails
# on the old code (session row never renders) and passes on the fix.
setup_twouser_test
fleet_up alpha

# The single provisioned instance's container id (same source as test 360).
container_id=$(grep -oE '"container_id":[[:space:]]*"[^"]+"' "${HOME}/.fleet/state.json" \
  | head -1 | grep -oE '"[^"]+"$' | tr -d '"')
[ -n "${container_id}" ] || fail "could not read container_id from state.json"
info "container_id = ${container_id}"

info "second user (app) starts its own tmux server first — the poison"
docker exec -u app "${container_id}" sh -c 'tmux new-session -d -s appsess sleep 600'

# Let fleetd's ~1s session poll probe (and, on the old code, cache the wrong
# user) before fleet's own session exists.
sleep 3

info "spawning TUI"
tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

info "expanding the instance session list"
tui_send j
sleep 0.5
tui_send Space
tui_wait_for "+ new session" 15

info "creating a fleet session (runs as the vscode remoteUser)"
tui_send a
tui_wait_for "session-name (or empty for auto)" 5
tui_send_text "mysession"
sleep 0.3
tui_send Enter
tui_wait_for "Session created" 15

# The session row is sourced from fleetd's poll (docker exec -u containerUser).
# It only renders if containerUser resolved to vscode — the session's owner — and
# not to app. This is the regression guard.
info "session must appear in the poll-fed list despite app's competing socket"
tui_wait_for "○ mysession" 20
tui_assert_contains "mysession" "fleet session invisible — containerUser resolved to the wrong user"

# app's foreign session lives under app's socket; fleet lists only its
# remoteUser's sessions, so it must not leak in.
tui_assert_not_contains "appsess" "a second user's tmux session leaked into fleet's list"

pass "session list resolves remoteUser despite a second user's tmux socket"
