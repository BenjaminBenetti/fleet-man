#!/usr/bin/env bash
# itest: no-docker
# Description: TUI new-fleet check passes through when repo has a devcontainer.json
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test

info "spawning TUI on empty fleet list"
tui_spawn
tui_wait_for "No instances" 15

info "opening new-fleet dialog and submitting a repo that already has a devcontainer.json"
tui_send n
tui_wait_for "New fleet" 5

tui_send_text "${FIXTURE_REPO_URL}"
tui_send Enter

info "verifying the no-devcontainer dialog is NEVER shown"
# Inspecting + add are both fast on a file:// clone, so the next visible
# state should be the "Added fleet" status message — not the warn
# dialog. The repo URL contains the fleet name, so we anchor on the
# explicit success message rather than the bare fleet name (which would
# also match the transient "Inspecting <URL>..." message).
tui_wait_for "Added fleet ${FIXTURE_REPO_NAME}" 15
tui_assert_not_contains "No devcontainer.json found" \
  "warn dialog appeared even though repo has a devcontainer.json"

info "verifying state persistence"
# `fleet ls` only lists instances, so a freshly-added empty fleet does
# not show up there. Read state.json directly instead.
assert_file_exists "${HOME}/.fleet/state.json"
state=$(cat "${HOME}/.fleet/state.json")
assert_contains "${state}" "\"${FIXTURE_REPO_NAME}\"" "fleet should be persisted to state.json"

pass "TUI new-fleet check passes through when devcontainer.json is present"
