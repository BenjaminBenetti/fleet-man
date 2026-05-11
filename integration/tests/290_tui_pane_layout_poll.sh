#!/usr/bin/env bash
# Description: Pane layout stays in sync with tmux via the discovery poll (%/Ctrl+Q coverage).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

# ---------------------------------------------------------------------------
# Regression test for the spotty-save behaviour in v0.16.0: saves only
# happened on close-toggle / switch / clean-quit, so panes added via %
# (which go through the outer tmux binding, not fleet) or panes killed
# via Ctrl+Q (same) left savedGroups and state.json stale. The fix
# piggybacks a re-snapshot onto the 1 s sessionDiscoveryMsg tick.
#
# This test exercises three assertions against state.json:
#
#   1. Opening a group with no close/quit eventually produces a saved
#      entry — i.e. the poll is actually running.
#   2. Adding a pane via `fleet shell --group` (what the % binding
#      does) bumps paneCount on the next tick.
#   3. An external kill-pane -a (what the Ctrl+Q binding does) does
#      NOT clobber the pre-kill snapshot — the last tick before the
#      kill captured it.
# ---------------------------------------------------------------------------

tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

tui_send j
sleep 0.5
tui_send Space
tui_wait_for "+ new session" 15

info "Open the first group pane"
tui_send Enter
tui_wait_for_pane 2 20

# Discover the group ID from the TUI — the random 6-hex token rendered
# on the session row. Same trick as 280_tui_pane_layout_persist.sh.
info "Wait for the group session row to render"
deadline=$(( $(date +%s) + 20 ))
group_id=""
while [ "$(date +%s)" -lt "${deadline}" ]; do
  screen=$(tui_capture_pane 0)
  candidate=$(printf '%s' "${screen}" | grep -oE '\<[a-f0-9]{6}\>' | head -1 || true)
  if [ -n "${candidate}" ]; then
    group_id="${candidate}"
    break
  fi
  sleep 0.5
done
if [ -z "${group_id}" ]; then
  printf -- '--- pane 0 ---\n%s\n--- end ---\n' "$(tui_capture_pane 0)" >&2
  fail "could not discover group id"
fi
info "group id: ${group_id}"

# ---------------------------------------------------------------------------
# Assertion 1: poll persists the group without any explicit close/quit.
#
# Before the fix, state.json would only gain groupLayouts[<gid>] after
# the user toggled the split off or quit fleet cleanly. With the poll,
# opening is enough: the next tick snapshots and saves.
# ---------------------------------------------------------------------------
info "Wait for discovery poll to persist the group (no close/quit)"
deadline=$(( $(date +%s) + 10 ))
found=""
while [ "$(date +%s)" -lt "${deadline}" ]; do
  if [ -f "${HOME}/.fleet/state.json" ] \
     && grep -q "\"${group_id}\"" "${HOME}/.fleet/state.json"; then
    found=1
    break
  fi
  sleep 0.5
done
[ -n "${found}" ] || fail "poll tick did not persist group before any close"
info "group recorded in state.json"

# ---------------------------------------------------------------------------
# Assertion 2: a pane added via `fleet shell --group` (what the %
# binding does) gets picked up on the next poll.
# ---------------------------------------------------------------------------
info "Add a second pane to the group (mimics %/\" binding)"
tmux split-window -h -t "${TUI_SESSION}:.1" \
  "env TERM=xterm-256color ${FLEET_BIN} shell ${FIXTURE_REPO_NAME}/alpha --group ${group_id}"
tui_wait_for_pane 3 20

info "Wait for discovery poll to capture new pane count"
deadline=$(( $(date +%s) + 15 ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  if grep -q '"paneCount": 2' "${HOME}/.fleet/state.json"; then
    break
  fi
  sleep 0.5
done
if ! grep -q '"paneCount": 2' "${HOME}/.fleet/state.json"; then
  printf -- '--- state.json ---\n%s\n--- end ---\n' "$(cat "${HOME}/.fleet/state.json")" >&2
  fail "discovery poll did not capture new pane count (expected paneCount: 2)"
fi
info "new pane count captured"

# Regression guard for PR #42's ghost-pane bug: under the old save logic
# the brief window between split-window and `fleet shell` tagging the
# pane title could persist a synthetic `~restored##` session. The strict
# save in derivePersistableSnapshot bails during that race instead.
assert_not_contains "$(cat "${HOME}/.fleet/state.json")" "~restored" \
  "state.json contains synthetic ~restored entries after pane add — PR #42 ghost-pane bug regressed"

# ---------------------------------------------------------------------------
# Assertion 3: an external kill-pane -a (what Ctrl+Q does) does NOT
# erase the pre-kill snapshot. The tick AFTER the kill detects
# !splitOpen() and clears the transient tracking fields, but
# savedGroups[<gid>] was already saved by the tick BEFORE the kill.
# ---------------------------------------------------------------------------
info "Kill split panes via tmux (what the Ctrl+Q binding does)"
tmux select-pane -t "${TUI_SESSION}:.0"
tmux kill-pane -a -t "${TUI_SESSION}"
tui_wait_for_pane 1 10

# Let at least one more discovery tick run so the TUI processes the
# external kill. The clear-transient-fields path should NOT touch
# groupLayouts — if a regression puts a save there, this check catches it.
sleep 2

state=$(cat "${HOME}/.fleet/state.json")
assert_contains "${state}" "\"${group_id}\"" \
  "group entry should survive external kill"
assert_contains "${state}" '"paneCount": 2' \
  "pre-kill pane count should survive external kill"
assert_not_contains "${state}" "~restored" \
  "post-kill state.json contains synthetic ~restored entries — PR #42 ghost-pane bug regressed"

pass "Pane layout syncs with tmux via discovery poll (%/Ctrl+Q coverage)"
