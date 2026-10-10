#!/usr/bin/env bash
# Description: tabs inside a session — in a session opened from the TUI, prefix+c (ctrl+x c in the split pane) creates a tab: the session gains a second tmux window, the in-pane tab bar appears, the session row shows "(2 tabs)", and prefix+p / n / 0 switch between the tabs.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"
session="alpha~tabs"

# session_field <format> — a tmux format for the session, read in the instance.
session_field() {
  "${FLEET_BIN}" exec "${inst}" -- tmux display-message -p -t "${session}" "$1" 2>/dev/null || true
}

# wait_session_field <format> <want> <what> — poll session_field for <want>.
wait_session_field() {
  local got="" deadline
  deadline=$(( $(date +%s) + $(_scale_timeout 15) ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    got=$(session_field "$1")
    [ "${got}" = "$2" ] && return 0
    sleep 0.5
  done
  fail "$3: ${1} is [${got}], want [$2]"
}

info "spawn-session alpha tabs"
"${FLEET_BIN}" spawn-session "${inst}" tabs >/dev/null || fail "spawn-session failed"

tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

info "expanding alpha and opening the tabs session"
tui_send j
sleep 0.5
tui_send Space
tui_wait_for "○ tabs" 15 || fail "the spawned session row never appeared"
tui_send j
sleep 0.5
tui_send Enter
tui_wait_for_pane 2 20 || fail "opening the session did not split a pane"

# The split pane runs the nested attach; once it is attached the session has a
# client and the inner prefix is ctrl+x.
wait_session_field '#{session_attached}' 1 "the split pane never attached to the session"
wait_session_field '#{prefix}' 'C-x' "the nested attach should set the session prefix to ctrl+x"
assert_equals "1" "$(session_field '#{session_windows}')" "a fresh session should have one tab"

info "prefix+c in the split pane creates a tab"
tui_send_pane 1 C-x
tui_send_pane 1 c
wait_session_field '#{session_windows}' 2 "prefix+c should create a second tmux window"
wait_session_field '#{window_index}' 1 "the new tab should be the active one"

# The tab bar is the inner status line (top of the pane), hidden while the
# session has one window and shown once it has more.
tui_wait_for_in_pane 1 "0:" 10 || fail "the tab bar should list tab 0"
tui_wait_for_in_pane 1 "1:" 10 || fail "the tab bar should list tab 1"
assert_equals "on" "$(session_field '#{status}')" "the tab bar should show with two tabs"

# The split took focus, so the TUI is pane 0.
tui_wait_for_in_pane 0 "(2 tabs)" 15 || fail "the session row should show its tab count"

info "prefix+p / n / 0 switch tabs"
tui_send_pane 1 C-x
tui_send_pane 1 p
wait_session_field '#{window_index}' 0 "prefix+p should switch to the previous tab"
tui_send_pane 1 C-x
tui_send_pane 1 n
wait_session_field '#{window_index}' 1 "prefix+n should switch to the next tab"
tui_send_pane 1 C-x
tui_send_pane 1 0
wait_session_field '#{window_index}' 0 "prefix+0 should switch to tab 0"

pass "prefix+c creates a tab (window, tab bar, row count) and prefix+p/n/0 switch tabs"
