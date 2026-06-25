#!/usr/bin/env bash
# Description: Opening a session group does not pull in a sibling group whose ID is a string prefix (dog vs dog-2)
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

# Repro for the "dog shows panes from both dog and dog-2" bug. The TUI names a
# group's sessions <instance>~<groupID> (root) and <instance>~<groupID>~<hex>
# (panes). Group operations used to filter membership with a raw
# strings.HasPrefix(name, "<instance>~<groupID>"), so the prefix "alpha~dog"
# also matched the sibling group "alpha~dog-2" (and its panes). Opening "dog"
# then split a pane for every "dog" AND "dog-2" session — the reported symptom.
#
# This test creates "dog" and "dog-2", opens "dog", and asserts exactly one
# shell pane appears (2 panes total incl. the TUI). On the buggy code "dog-2"
# leaks in as a second shell pane (3 total) and the assertion fails.
setup_test
fleet_up alpha

info "spawning TUI"
tui_spawn

info "waiting for TUI to hydrate before any keypress"
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

info "moving cursor to alpha instance row and expanding its session list"
tui_send j
sleep 0.5
tui_send Space
tui_wait_for "+ new session" 15

# --- create session "dog" ---
info "creating session 'dog'"
tui_send a
tui_wait_for "session-name (or empty for auto)" 5
tui_send_text "dog"
sleep 0.3
tui_send Enter
tui_wait_for "Session created" 15
tui_wait_for "○ dog" 15

# --- create sibling session "dog-2" (cursor is still on the instance row) ---
info "creating sibling session 'dog-2'"
tui_send a
tui_wait_for "session-name (or empty for auto)" 5
tui_send_text "dog-2"
sleep 0.3
tui_send Enter
tui_wait_for "Session created" 15
# Wait for the dog-2 row AND for the server's ~1s poll to surface it in the
# runtime — opening "dog" reads that runtime session list, so both sessions
# must be live there for the bug (if present) to manifest.
tui_wait_for "○ dog-2" 15
sleep 2

# --- open the "dog" group ---
# Rows under the expanded instance are sorted by group ID: "dog" then "dog-2".
# From the instance row a single 'j' lands on "dog".
info "opening the 'dog' group"
tui_send j
sleep 0.5
tui_send Enter

# The split opens asynchronously; wait for it to appear.
info "waiting for the dog split pane to open"
opened=0
deadline=$(( $(date +%s) + $(_scale_timeout 20) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  [ "$(tui_pane_count)" -ge 2 ] && { opened=1; break; }
  sleep 0.25
done
[ "${opened}" -eq 1 ] || fail "the 'dog' group never opened a split pane"

# restoreGroupCmd creates every pane synchronously, so the count settles fast.
# Give a leaked dog-2 pane time to materialize before asserting the final count.
sleep 3
panes="$(tui_pane_count)"
assert_equals 2 "${panes}" \
  "opening 'dog' produced ${panes} panes; expected 2 (TUI + dog) — the 'dog-2' group leaked in via a prefix-matched group restore"

# Belt-and-suspenders: a leaked pane is titled with the dog-2 session name.
titles="$(tmux list-panes -t "${TUI_SESSION}" -F '#{pane_title}' 2>/dev/null || true)"
assert_not_contains "${titles}" "dog-2" \
  "the dog-2 session leaked into the dog group's split panes (titles: ${titles})"

pass "opening a session group ignores a sibling group whose ID is a prefix"
