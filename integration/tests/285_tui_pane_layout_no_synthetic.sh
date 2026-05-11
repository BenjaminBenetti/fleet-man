#!/usr/bin/env bash
# Description: Save path never fabricates synthetic ~restored session names during the title-tag race.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

# ---------------------------------------------------------------------------
# Regression guard for the PR #42 ghost-pane bug.
#
# PR #42's normalizeSavedGroupSessions mapped any non-parseable outer-tmux
# pane title (e.g. the runner hostname a brand-new pane defaults to before
# `fleet shell` runs `tmux select-pane -T`) into a synthetic
# `<instance>~<groupID>~restored##` session name. The fabricated name was
# persisted in state.json; on restore `fleet shell --session <name>` then
# created that tmux session, producing a blank terminal the user could not
# remove.
#
# The fix replaces normalizeSavedGroupSessions with derivePersistableSnapshot,
# which bails when any pane has an empty/non-group title — the 250ms layout
# tick simply retries on the next firing instead of poisoning the persisted
# state. This test exercises the exact window the old logic corrupted: open
# a group with one pane, rapidly add a second via the same path the outer-
# tmux %/" binding uses (`fleet shell --group <gid>`), and watch state.json
# for any synthetic entries through the race.
# ---------------------------------------------------------------------------

info "Launch TUI and expand alpha"
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

# Discover the group ID off the screen — same trick as tests 280/290.
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
  fail "could not discover group id from TUI"
fi
info "group id: ${group_id}"

# Add the second pane and watch state.json through the title-tag race.
# Each iteration grep's the live file — if any save tick during the race
# wrote a synthetic entry, this catches it before the next clean tick can
# overwrite it. Poll faster than the 250ms layoutTickMsg to maximise
# detection of a transient corrupt save.
info "Add a second pane and watch state.json for synthetic entries"
tmux split-window -h -t "${TUI_SESSION}:.1" \
  "env TERM=xterm-256color ${FLEET_BIN} shell ${FIXTURE_REPO_NAME}/alpha --group ${group_id}"
tui_wait_for_pane 3 20

watch_deadline=$(( $(date +%s) + $(_scale_timeout 10) ))
while [ "$(date +%s)" -lt "${watch_deadline}" ]; do
  if [ -f "${HOME}/.fleet/state.json" ] \
     && grep -q "~restored" "${HOME}/.fleet/state.json"; then
    printf -- '--- state.json ---\n%s\n--- end ---\n' \
      "$(cat "${HOME}/.fleet/state.json")" >&2
    fail "state.json wrote a synthetic ~restored entry during the title-tag race"
  fi
  sleep 0.1
done

# Final invariant: the persisted state must never contain ~restored, even
# after the race has fully settled. The watch loop above catches transient
# bad writes; this catches a save that landed and stuck.
info "Verify final state.json"
assert_file_exists "${HOME}/.fleet/state.json"
state=$(cat "${HOME}/.fleet/state.json")
# Dump for diagnostics — if a future regression hits here, the log will
# show exactly what was on disk without needing to re-run.
printf -- '--- state.json ---\n%s\n--- end ---\n' "${state}" >&2
assert_not_contains "${state}" "~restored" \
  "final state.json contains synthetic ~restored entries — PR #42 ghost-pane bug regressed"

pass "Save path never persists synthetic ~restored session names"
