#!/usr/bin/env bash
# Description: Layout presets (issue #150) — capture a preset from a live session, then create a templated session whose pane command auto-runs
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

# ---------------------------------------------------------------------------
# Phase 1 — create the source session the preset will be captured from.
# ---------------------------------------------------------------------------
info "creating source session 'src'"
tui_send a
tui_wait_for "session-name (or empty for auto)" 5
tui_send_text "src"
sleep 0.3
tui_send Enter
tui_wait_for "Session created" 15
tui_wait_for "○ src" 15

# ---------------------------------------------------------------------------
# Phase 2 — capture a preset from it in the edit-fleet Layouts section.
# ---------------------------------------------------------------------------
info "opening the edit-fleet dialog"
tui_send k
sleep 0.3
tui_send e
tui_wait_for "Edit fleet" 5
tui_assert_contains "Layouts (0)" "Layouts section header missing"

info "expanding Layouts and starting preset creation"
for _ in 1 2 3 4 5 6; do tui_send j; sleep 0.1; done
tui_send l
tui_wait_for "+ Layout Preset" 5
tui_send j
sleep 0.2
tui_send Enter
tui_wait_for "Capture the layout of:" 5
tui_assert_contains "alpha / src" "source session candidate missing"

info "capturing the session and assigning the pane command"
tui_send Enter
# Edit stage: focus starts on pane 1; [ save ] is always offered (no gating).
tui_wait_for "pane 1:" 5
tui_assert_contains "[ save ]" "save button should always be shown"
tui_send Enter
tui_wait_for "Pane 1 command:" 5
tui_send_text "echo preset-pane-ready"
sleep 0.3
tui_send Enter
# Single pane: setting its command advances focus to [ save ].
tui_wait_for "[ save ]" 5
sleep 0.3

info "saving the preset"
tui_send Enter
tui_wait_for "Layouts (1)" 10
tui_assert_contains "src (1 pane)" "saved preset row missing"

info "preset persisted to fleet settings"
assert_file_exists "${HOME}/.fleet/state.json"
state=$(cat "${HOME}/.fleet/state.json")
assert_contains "${state}" '"layoutPresets"' "state.json missing layoutPresets"
assert_contains "${state}" 'preset-pane-ready' "state.json missing the pane command"

info "closing the edit-fleet dialog"
tui_send Escape
sleep 0.5

# ---------------------------------------------------------------------------
# Phase 3 — create a new session from the template via Tab cycling.
# ---------------------------------------------------------------------------
info "opening the New session dialog on the instance"
tui_send j
sleep 0.3
tui_send a
tui_wait_for "Template:" 5
tui_assert_contains "(none)" "template should default to none"

info "Tab-selecting the preset and naming the session"
tui_send Tab
tui_wait_for "src (1 pane)" 5
tui_send_text "fromtpl"
sleep 0.3
tui_send Enter
tui_wait_for "Session fromtpl created (1 pane)" 20
tui_wait_for "○ fromtpl" 15

info "templated session records a group layout in state.json"
deadline=$(( $(date +%s) + 10 ))
ok=""
while [ "$(date +%s)" -lt "${deadline}" ]; do
  if grep -q '"fromtpl"' "${HOME}/.fleet/state.json" 2>/dev/null; then
    ok=1
    break
  fi
  sleep 0.5
done
[ -n "${ok}" ] || fail "state.json never recorded the fromtpl group layout"

# ---------------------------------------------------------------------------
# Phase 4 — the preset's startup command actually ran in the new pane.
# ---------------------------------------------------------------------------
info "verifying the startup command ran inside the inner session"
deadline=$(( $(date +%s) + 20 ))
ok=""
while [ "$(date +%s)" -lt "${deadline}" ]; do
  out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- tmux capture-pane -p -t '=alpha~fromtpl:' 2>/dev/null || true)
  if printf '%s' "${out}" | grep -q "preset-pane-ready"; then
    ok=1
    break
  fi
  sleep 1
done
if [ -z "${ok}" ]; then
  printf -- '--- inner pane capture ---\n%s\n--- end ---\n' "${out:-}" >&2
  fail "preset startup command output not found in the templated session"
fi

pass "TUI layout preset capture + templated session creation"
