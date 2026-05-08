#!/usr/bin/env bash
# Description: After tagging an instance, the tag is rendered in the instance row and survives a TUI restart.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

# ===========================================
# Spawn TUI, wait for the instance row to fully hydrate before sending
# any keys (matches the pattern used by other tag/rename tests so we
# don't race the build-rows pass).
# ===========================================
tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

# Before tagging, the row must NOT contain a `# ` segment — otherwise the
# post-tag assertion would be meaningless.
tui_assert_not_contains "# release-blocker" "tag should not be present yet"

# ===========================================
# Tag the instance via 't'.
# ===========================================
info "moving cursor to alpha"
tui_send j
sleep 0.3

info "opening tag dialog"
tui_send t
tui_wait_for "Tag instance" 5

info "submitting tag 'release-blocker'"
tui_send_text "release-blocker"
sleep 0.3
tui_send Enter
tui_wait_for "Tagged itest-fleet/alpha: release-blocker" 5
tui_wait_for_absent "Tag instance" 5

# ===========================================
# 1. The tag must be rendered in the row. page_fleet renders it as
#    `  # <tag>` appended to the instance line. Capture once and assert
#    on the literal token — the dim style does not change the substring.
# ===========================================
info "asserting tag visible in row"
tui_assert_contains "# release-blocker" "tag should be rendered in instance row after save"

# Persistence in state.json — same contract as test 210, kept here so
# this test is self-contained.
tag_value=$(grep -oE '"tag"[[:space:]]*:[[:space:]]*"[^"]*"' "${HOME}/.fleet/state.json" | head -1 || true)
assert_contains "${tag_value}" "release-blocker" "tag missing from state.json after save"

# ===========================================
# 2. Quit + relaunch the TUI; the tag must still render.
# ===========================================
info "quitting TUI"
tui_send q
deadline=$(( $(date +%s) + 10 ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  if ! tmux has-session -t "${TUI_SESSION}" 2>/dev/null; then
    break
  fi
  sleep 0.25
done
if tmux has-session -t "${TUI_SESSION}" 2>/dev/null; then
  fail "TUI did not exit after q"
fi

info "respawning TUI"
tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

info "asserting tag visible after relaunch"
tui_assert_contains "# release-blocker" "tag should still render after TUI relaunch"

pass "TUI tag is rendered in the row and persists across restart"
