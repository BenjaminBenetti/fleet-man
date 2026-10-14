#!/usr/bin/env bash
# itest: k8s
# Description: TUI launches, renders the logo, fleet name, and running instance.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

info "Spawning TUI"
tui_spawn

info "Waiting for fleet header to render"
tui_wait_for "itest-fleet" 15

# Logo strips to the "fleet" word as text — the gradient lettering
# uses block chars so just assert on the fleet/instance fragments.
tui_assert_contains "itest-fleet"           "fleet header missing"
tui_assert_contains "alpha"                 "instance row missing"
tui_assert_contains "running"               "instance status missing"
tui_assert_contains "settings"              "settings row missing"

pass "TUI startup renders fleet list"
