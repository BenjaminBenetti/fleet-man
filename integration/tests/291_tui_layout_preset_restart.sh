#!/usr/bin/env bash
# Description: Layout presets survive a full fleetd restart (issue #150 persistence) — create a preset, kill the daemon + TUI, relaunch, and confirm the preset is still there.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

info "spawning TUI"
tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

info "expanding the instance session list"
tui_send j
sleep 0.5
tui_send Space
tui_wait_for "+ new session" 15

info "creating source session 'src'"
tui_send a
tui_wait_for "session-name (or empty for auto)" 5
tui_send_text "src"
sleep 0.3
tui_send Enter
tui_wait_for "Session created" 15
tui_wait_for "○ src" 15

info "opening the edit-fleet dialog and capturing a preset"
tui_send k
sleep 0.3
tui_send e
tui_wait_for "Edit fleet" 5
# From the dialog's top row (the collapsed Agents header, issue #184), Layouts
# is 4 rows down: Agents → GitHub CLI → Home dir → Prefer Fleet Launch → Layouts.
for _ in 1 2 3 4; do tui_send j; sleep 0.1; done
tui_send l
tui_wait_for "+ Layout Preset" 5
tui_send j
sleep 0.2
tui_send Enter
tui_wait_for "Capture the layout of:" 5
tui_send Enter
tui_wait_for "pane 1:" 5
tui_send Enter
tui_wait_for "Pane 1 command:" 5
tui_send_text "echo persisted-across-restart"
sleep 0.3
tui_send Enter
tui_wait_for "[ save ]" 5
sleep 0.3
tui_send Enter
tui_wait_for "Layouts (1)" 10

info "preset is in state.json before restart"
assert_file_exists "${HOME}/.fleet/state.json"
assert_contains "$(cat "${HOME}/.fleet/state.json")" 'persisted-across-restart' \
  "state.json missing the preset command before restart"

# ---------------------------------------------------------------------------
# Fully close fleet: kill the TUI AND the fleetd daemon (the user's scenario).
# Reopening then auto-spawns a FRESH daemon that must load the preset back from
# state.json and serve it to the reconnecting TUI.
# ---------------------------------------------------------------------------
info "killing the TUI and the fleetd daemon"
tui_kill
pkill -f "${FLEET_BIN} server" 2>/dev/null || true
# Wait for the daemon socket to disappear so the relaunch truly respawns a
# fresh process. If it never goes away the kill was a no-op and this test would
# silently degrade into "reconnect to the same daemon" — fail loudly instead,
# so a green result always means a real cold reload from state.json.
deadline=$(( $(date +%s) + 15 ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  [ -S "${HOME}/.fleet/fleet.sock" ] || break
  sleep 0.25
done
if [ -S "${HOME}/.fleet/fleet.sock" ]; then
  fail "fleetd socket still present — daemon was not killed, restart not exercised"
fi
info "daemon is down (socket gone); the relaunch will spawn a fresh one"

info "relaunching the TUI (respawns a fresh daemon from state.json)"
tui_spawn
tui_wait_for "alpha" 30
tui_wait_for "○ idle" 60

info "opening edit-fleet again — the preset must still be there"
tui_send e
tui_wait_for "Edit fleet" 5
tui_assert_contains "Layouts (1)" "preset count lost after restart — the layout preset did not persist"

info "expanding Layouts to confirm the preset row survived"
# Layouts is 4 rows below the collapsed Agents header (issue #184).
for _ in 1 2 3 4; do tui_send j; sleep 0.1; done
tui_send l
sleep 0.5
tui_assert_contains "src (1 pane)" "preset row missing after restart"

pass "layout preset survived a full fleetd restart"
