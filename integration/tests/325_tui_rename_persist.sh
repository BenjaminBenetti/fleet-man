#!/usr/bin/env bash
# Description: Renamed instance display_name renders in the row and survives a TUI restart.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

# ===========================================
# Rename alpha → triage via the edit dialog. This mirrors test 215 but
# focuses on the rendered display name rather than just state.json,
# and additionally asserts the new name survives a TUI relaunch.
# ===========================================
tui_spawn
tui_wait_for "alpha"  15
tui_wait_for "○ idle" 60

info "moving cursor to alpha"
tui_send j
sleep 0.3

info "opening edit dialog"
tui_send e
tui_wait_for "Edit instance" 5

info "clearing prefilled name and submitting 'triage'"
for _ in 1 2 3 4 5; do tui_send BSpace; done
tui_send_text "triage"
sleep 0.3
tui_send Enter

info "asserting new label appears immediately"
tui_wait_for_absent "Edit instance" 5
tui_wait_for "triage" 5

# state.json is the source of truth — verify display_name persisted.
display_value=$(grep -oE '"display_name"[[:space:]]*:[[:space:]]*"[^"]*"' "${HOME}/.fleet/state.json" \
  | head -1 || true)
assert_contains "${display_value}" "triage" "display_name should persist in state.json"

# Underlying instance name must NOT change (rename is display-only).
name_value=$(grep -oE '"name"[[:space:]]*:[[:space:]]*"alpha"' "${HOME}/.fleet/state.json" \
  | head -1 || true)
assert_contains "${name_value}" "alpha" "underlying instance name must remain 'alpha'"

# ===========================================
# Quit + relaunch. The rendered row must continue to use the renamed
# label rather than reverting to the underlying instance name.
# ===========================================
info "quitting TUI"
tui_send q
deadline=$(( $(date +%s) + 10 ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  tmux has-session -t "${TUI_SESSION}" 2>/dev/null || break
  sleep 0.25
done
if tmux has-session -t "${TUI_SESSION}" 2>/dev/null; then
  fail "TUI did not exit after q"
fi

info "respawning TUI"
tui_spawn
tui_wait_for "triage" 15
tui_wait_for "○ idle" 60

info "asserting renamed row visible after restart"
tui_assert_contains "triage" "display name should still render after TUI relaunch"

# CLI ls reads from state.json — it shows the underlying name, not the
# display label. This is an intentional split (CLI uses stable identifiers,
# TUI shows user-facing labels). Lock that contract in.
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_out}" "alpha" "CLI ls should list the underlying instance name"

pass "TUI rename persists across restart and stays distinct from underlying name"
