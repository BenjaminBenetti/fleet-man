#!/usr/bin/env bash
# itest: no-docker
# Description: TUI new-fleet abort path when repo lacks a devcontainer.json
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

# Use the dedicated no-devcontainer fixture for this test instead of the
# default debian fixture. setup_test reads $FIXTURE_SRC, so overriding
# it inline is enough.
FIXTURE_SRC="${INTEGRATION_DIR}/fixture-no-devcontainer" setup_test

info "spawning TUI on empty fleet list"
tui_spawn
tui_wait_for "No instances" 15

info "opening new-fleet dialog and submitting a repo without a devcontainer.json"
tui_send n
tui_wait_for "New fleet" 5

tui_send_text "${FIXTURE_REPO_URL}"
tui_send Enter

info "waiting for the no-devcontainer warning to appear"
tui_wait_for "No devcontainer.json found" 15
tui_assert_contains "Abort" "Abort option missing from warn dialog"
tui_assert_contains "Setup" "Setup option missing from warn dialog"

info "choosing Abort with 'a'"
tui_send a
tui_wait_for_absent "No devcontainer.json found" 5

info "verifying the empty state is preserved and no fleet was added"
tui_wait_for "No instances" 5
# A fresh fleet with no instances would be invisible to `fleet ls`, so
# the only reliable signal that Abort really refused to persist is to
# look at state.json itself.
if [ -f "${HOME}/.fleet/state.json" ]; then
  state=$(cat "${HOME}/.fleet/state.json")
  assert_not_contains "${state}" "\"${FIXTURE_REPO_NAME}\"" \
    "no fleet should have been persisted after Abort"
fi

pass "TUI new-fleet abort leaves no trace when devcontainer.json is missing"
