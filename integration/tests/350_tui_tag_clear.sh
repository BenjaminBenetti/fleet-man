#!/usr/bin/env bash
# Description: Re-opening the tag dialog with empty input clears the tag line from the expanded instance and from state.json.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha"  15
tui_wait_for "○ idle" 60

# ===========================================
# Step 1 — set a tag the test will later clear. The tag renders on its
# own line under the instance row, but only while the instance is
# expanded, so expand alpha before asserting "# wip" is visible.
# ===========================================
TAG="wip"
info "moving cursor to alpha"
tui_send j
sleep 0.3

info "opening tag dialog and saving '${TAG}'"
tui_send t
tui_wait_for "Tag instance" 5
tui_send_text "${TAG}"
sleep 0.3
tui_send Enter
tui_wait_for "Tagged itest-fleet/alpha: ${TAG}" 5
tui_wait_for_absent "Tag instance" 5

info "expanding alpha"
tui_send Space
tui_wait_for "+ new session" 15
tui_assert_contains "# ${TAG}" "tag must render under the expanded instance row after save"

# state.json sanity — the tag field is "wip".
tag_value=$(grep -oE '"tag"[[:space:]]*:[[:space:]]*"[^"]*"' "${HOME}/.fleet/state.json" | head -1 || true)
assert_contains "${tag_value}" "${TAG}" "tag should be in state.json after save"

# ===========================================
# Step 2 — re-open the dialog. The text input is prefilled with the
# current tag (SetValue(inst.Tag) in the 't' handler), so we backspace
# every character to leave it empty, then submit.
# ===========================================
# Defensive cursor re-seat — after dialog close, cursor stays on alpha.
# The j/k wiggle also exercises the cursor skipping the (non-selectable)
# tag line that now sits directly under alpha.
tui_send j
sleep 0.2
tui_send k
sleep 0.2

info "re-opening tag dialog (prefilled with '${TAG}')"
tui_send t
tui_wait_for "Tag instance" 5

info "backspacing the prefilled tag"
# One backspace per character in TAG (3 chars), plus a couple extra for
# safety against any leading whitespace.
for _ in 1 2 3 4 5; do tui_send BSpace; done
sleep 0.3
tui_send Enter

# The handler emits "Cleared tag for ..." when the trimmed value is empty.
info "waiting for 'Cleared tag' confirmation"
tui_wait_for "Cleared tag for itest-fleet/alpha" 5
tui_wait_for_absent "Tag instance" 5

# ===========================================
# Step 3 — the tag line must be gone even though the instance is still
# expanded. The status message itself contains "wip" only via "Cleared
# tag for itest-fleet/alpha" — which does NOT include the tag — so plain
# absence is safe.
# ===========================================
info "verifying tag line no longer rendered"
tui_assert_not_contains "# ${TAG}" "tag line must be gone after clear"

# state.json: tag field is the empty string.
tag_value=$(grep -oE '"tag"[[:space:]]*:[[:space:]]*"[^"]*"' "${HOME}/.fleet/state.json" | head -1 || true)
# Either the field is absent (nothing matched) or it's `"tag": ""`.
if [ -n "${tag_value}" ]; then
  assert_contains "${tag_value}" '""' "tag should be empty in state.json after clear"
fi

# Persistence check — relaunch the TUI, re-expand, and verify the tag
# line stays gone.
info "quitting TUI"
tui_send q
deadline=$(( $(date +%s) + 10 ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  tmux has-session -t "${TUI_SESSION}" 2>/dev/null || break
  sleep 0.25
done
tmux has-session -t "${TUI_SESSION}" 2>/dev/null && fail "TUI did not exit after q"

info "respawning TUI"
tui_spawn
tui_wait_for "alpha"  15
tui_wait_for "○ idle" 60
tui_send j
sleep 0.3
tui_send Space
tui_wait_for "+ new session" 15
tui_assert_not_contains "# ${TAG}" "cleared tag must stay cleared after restart"

pass "TUI tag-clear round-trip removes the tag line + state and persists"
