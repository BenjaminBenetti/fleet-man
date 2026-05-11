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
# PR #42's normalizeSavedGroupSessions used to map any non-parseable pane
# title (e.g. the runner hostname that a brand-new tmux pane defaults to
# before `fleet shell` runs `tmux select-pane -T`) into a synthetic
# `<instance>~<groupID>~restored##` session name. The fabricated name was
# then persisted in state.json. On the next restore, `fleet shell
# --session <name>` would create that tmux session if it didn't exist —
# producing a blank terminal that the user could not get rid of.
#
# The fix replaces normalizeSavedGroupSessions with a strict
# derivePersistableSnapshot that bails whenever any pane has an empty or
# non-group title. This test exercises the exact window the old logic
# corrupted: open a group with one pane, then rapidly add a second pane
# via the same path the outer-tmux %/" binding uses (`fleet shell --group
# <gid>`), and watch state.json for synthetic entries throughout the
# title-tag race.
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

# Watch state.json continuously while we add the second pane. The bug
# would write `~restored` for any tick that fires after split-window but
# before `fleet shell` tags the new pane's title — usually a window of
# 100ms to several seconds depending on host/devcontainer speed. Even on
# a fast runner the 250ms layoutTickMsg fires inside that window often
# enough to catch the regression reliably.
info "Add a second pane and watch state.json for synthetic entries"
tmux split-window -h -t "${TUI_SESSION}:.1" \
  "env TERM=xterm-256color ${FLEET_BIN} shell ${FIXTURE_REPO_NAME}/alpha --group ${group_id}"
tui_wait_for_pane 3 20

# Poll for up to 10 seconds (scaled). On every iteration grep the live
# file — if any save tick during the race wrote a synthetic entry the
# next tick can overwrite it with a clean snapshot, so polling at a
# higher rate than the 250ms tick maximises detection.
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

# ---------------------------------------------------------------------------
# Sanity: by now titles have settled and the save should reflect 2 real
# panes with names that parse as belonging to this group. A non-matching
# session name would be a different kind of fabrication.
# ---------------------------------------------------------------------------
state=$(cat "${HOME}/.fleet/state.json")
assert_contains "${state}" "\"${group_id}\"" \
  "state.json missing entry for group ${group_id}"
assert_contains "${state}" '"paneCount": 2' \
  "state.json should record 2 shell panes after title-tag settle"
assert_not_contains "${state}" "~restored" \
  "post-settle state.json contains synthetic ~restored entries"

# Every persisted session name must start with `alpha~${group_id}`. Names
# that don't belong to this group would imply fabrication or cross-group
# contamination.
sessions_blob=$(printf '%s' "${state}" \
  | grep -oE '"sessions"[[:space:]]*:[[:space:]]*\[[^]]*\]' \
  | head -1)
[ -n "${sessions_blob}" ] || fail "could not locate sessions array in state.json"
info "sessions: ${sessions_blob}"
expected_prefix="alpha~${group_id}"
names=$(printf '%s' "${sessions_blob}" | grep -oE '"[^"]+"' | sed 's/^"//;s/"$//')
while IFS= read -r name; do
  [ -z "${name}" ] && continue
  case "${name}" in
    "${expected_prefix}"*) ;;
    *)
      printf -- '--- state.json ---\n%s\n--- end ---\n' "${state}" >&2
      fail "saved session [${name}] does not belong to group ${group_id}"
      ;;
  esac
done <<< "${names}"

pass "Save path never persists synthetic ~restored session names"
