#!/usr/bin/env bash
# itest: k8s
# Description: Full TUI lifecycle — create fleet, create instance, shell in (split pane), run cmd, delete instance.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

# Fresh state — no fleets, no instances. The TUI will build it from scratch.
setup_test

info "Spawn the TUI with no fleets"
tui_spawn
tui_wait_for "No instances" 15

# ---------------------------------------------------------------------------
# Step 1 — create the fleet via `n`.
# ---------------------------------------------------------------------------
info "Press 'n' to open the new-fleet dialog"
tui_send n
tui_wait_for "git@github.com" 5   # placeholder visible in the dialog

info "Type the fixture repo URL"
tui_send_text "${FIXTURE_REPO_URL}"
sleep 0.3
tui_send Enter
tui_wait_for "Added fleet ${FIXTURE_REPO_NAME}" 5
tui_wait_for "${FIXTURE_REPO_NAME}" 5

# ---------------------------------------------------------------------------
# Step 2 — create an instance via `a`.
# ---------------------------------------------------------------------------
info "Move cursor to the fleet header (j/k wraps) and press 'a'"
# After 'n' the cursor may have stayed on the settings row; press k
# once so we land on the freshly-added fleet header.
tui_send k
sleep 0.3
tui_send a
tui_wait_for "instance-name" 5

info "Name the instance 'alpha' and submit"
tui_send_text "alpha"
sleep 0.3
tui_send Enter

info "Wait for the instance to finish provisioning"
# The transitional "creating" status resolves to "running" when devcontainer
# up completes. Wait for the hydrated row — the `○ idle` agent indicator
# only appears once the post-start poll has reported the row as running.
# (Just waiting for "running" can match the one-shot status message that
# fires a beat before the row itself transitions.)
tui_wait_for "Creating ${FIXTURE_REPO_NAME}/alpha" 5 || true   # may flash past
tui_wait_for "○ idle" 120
tui_assert_contains "alpha" "instance row missing after create"

# ---------------------------------------------------------------------------
# Step 3 — shell into the instance via `enter` (opens split pane).
# ---------------------------------------------------------------------------
info "Move cursor to the alpha instance and press enter to open a split pane"
tui_send j
sleep 0.3
tui_send Enter

# A second pane should appear in the tmux window.
tui_wait_for_pane 2 20

# ---------------------------------------------------------------------------
# Step 4 — run a shell command in the split pane and verify the output.
# ---------------------------------------------------------------------------
# The inner pane runs tmux-inside-the-container. Give its shell a moment
# to become ready (tmux session attach + container shell spawn).
sleep 3
SENTINEL="hello-from-splitpane-$(date +%s)"
info "Run 'echo ${SENTINEL}' inside the split pane"
tui_send_pane_text 1 "echo ${SENTINEL}"
tui_send_pane 1 Enter

info "Verify the output appeared in the pane"
tui_wait_for_in_pane 1 "${SENTINEL}" 15

# Also confirm we are actually inside the container (not the host shell)
# by checking the working directory reflects the mounted workspace.
tui_send_pane_text 1 "pwd"
tui_send_pane 1 Enter
tui_wait_for_in_pane 1 "/workspaces/${FIXTURE_REPO_NAME}" 10

# ---------------------------------------------------------------------------
# Step 5 — close the split pane and delete the instance via `d` + `y`.
# ---------------------------------------------------------------------------
info "Close the split pane so we can interact with the TUI again"
# We could send Ctrl+Q (bound by fleet to kill-pane -a), but tmux
# send-keys bypasses key bindings — the C-q character flows straight
# into the TUI's own "quit" handler, exiting the TUI. Kill the pane
# directly via tmux instead, which is what the C-q binding does anyway.
tmux kill-pane -t "${TUI_SESSION}:.1" 2>/dev/null || true
tui_wait_for_pane 1 5

info "Move back to alpha (cursor should still be on it) and press 'd'"
# Ensure cursor is on alpha. Building rows may have reordered; press k
# once to move up from settings row just in case, then j to re-seat.
tui_send k
sleep 0.2
tui_send j
sleep 0.2
tui_send d
tui_wait_for "Delete instance" 5
tui_wait_for "Remove ${FIXTURE_REPO_NAME}/alpha" 5

info "Confirm deletion with 'y'"
tui_send y
# Async deletion: transitions to "deleting", then the row disappears and
# the fleet header's instance count drops to 0. The word "alpha" itself
# still appears transiently in the "Removed itest-fleet/alpha" status
# message, so assert on the empty-fleet header / success message instead.
tui_wait_for "Removed ${FIXTURE_REPO_NAME}/alpha" 30
tui_wait_for "${FIXTURE_REPO_NAME} (0)" 10

# Verify via CLI that state actually reflects the removal.
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" || true)
assert_not_contains "${ls_out}" "alpha" "CLI ls should not list alpha after TUI delete"

pass "TUI full lifecycle (create fleet → instance → shell → verify → delete)"
