#!/usr/bin/env bash
# itest: no-docker
# Description: TUI new-fleet Setup branch adds the fleet and launches the agent
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-no-devcontainer" setup_test

# Install a stub `claude` binary that records its argv and exits 0. The
# stub fronts any real claude/codex/copilot install on the runner so the
# Setup path is deterministic: we can assert the agent ran without
# needing a real interactive session and without the test hanging.
AGENT_MARKER="${TEST_SCRATCH_DIR}/agent-invoked"
install_stub_agent "${AGENT_MARKER}"

info "spawning TUI on empty fleet list"
tui_spawn
tui_wait_for "No instances" 15

info "opening new-fleet dialog and submitting a repo without a devcontainer.json"
tui_send n
tui_wait_for "New fleet" 5

tui_send_text "${FIXTURE_REPO_URL}"
tui_send Enter

info "waiting for the no-devcontainer warning dialog"
tui_wait_for "No devcontainer.json found" 15

info "choosing Setup with 's'"
tui_send s

# The TUI hands the terminal off to the stub agent via tea.ExecProcess.
# The stub exits immediately, so bubbletea regains control and rerenders
# the fleet list with the new fleet visible. Assert both outcomes:
#   1. The agent was actually invoked (stub wrote its marker).
#   2. The fleet was persisted optimistically before the agent ran.

info "waiting for the stub agent to be invoked"
deadline=$(( $(date +%s) + $(_scale_timeout 15) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  [ -f "${AGENT_MARKER}" ] && break
  sleep 0.25
done
assert_file_exists "${AGENT_MARKER}"

marker_contents=$(cat "${AGENT_MARKER}")
assert_contains "${marker_contents}" "${FIXTURE_REPO_URL}" \
  "agent prompt should reference the repo URL it must clone"
assert_contains "${marker_contents}" "DEVCONTAINER_SETUP.md" \
  "agent prompt should reference the devcontainer setup skill URL"

info "verifying the fleet was added immediately on Setup (before the agent finished)"
# `fleet ls` only lists instances — a new empty fleet does not surface
# there. Probe state.json directly: addPendingFleet persists the record
# synchronously before tea.ExecProcess hands off to the agent, so the
# write must be visible on disk by the time the stub exited.
assert_file_exists "${HOME}/.fleet/state.json"
state=$(cat "${HOME}/.fleet/state.json")
assert_contains "${state}" "\"${FIXTURE_REPO_NAME}\"" \
  "fleet should be persisted as soon as Setup is chosen"

pass "TUI new-fleet Setup branch adds the fleet and launches the configured agent"
