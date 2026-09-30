#!/usr/bin/env bash
# Description: Saved session groups stay with their fleet when instance names match (#255).
# itest: no-docker
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin
setup_test

# No containers are needed: saved groups must render independently even before
# runtime discovery. Both fleets deliberately use the same instance name.
cat > "${HOME}/.fleet/state.json" <<'JSON'
{
  "fleets": {
    "one": {"name":"one","instances":[{"name":"alpha","status":"running"}]},
    "two": {"name":"two","instances":[{"name":"alpha","status":"running"}]}
  },
  "groupLayouts": {
    "one/alpha/first": {"fleetName":"one","instanceName":"alpha","groupID":"first","sessions":["alpha~first"],"paneCount":1},
    "two/alpha/second": {"fleetName":"two","instanceName":"alpha","groupID":"second","sessions":["alpha~second"],"paneCount":1}
  }
}
JSON

tui_spawn
tui_wait_for "alpha" 15
tui_send j
tui_send Space
tui_wait_for "first" 10
tui_assert_not_contains "second" "one/alpha must not show two/alpha's saved session"

# Collapse the first instance, then walk past the second fleet header and
# expand its identically named instance.
tui_send Space
tui_wait_for_absent "first" 10
tui_send j
tui_send j
tui_send Space
tui_wait_for "second" 10
tui_assert_not_contains "first" "two/alpha must not show one/alpha's saved session"

pass "same-named instances keep saved sessions in their own fleets"
