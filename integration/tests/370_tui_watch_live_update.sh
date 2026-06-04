#!/usr/bin/env bash
# Description: a new instance created via the CLI appears in a running TUI WITHOUT pressing `r` (Watch push).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha" 15
tui_assert_not_contains "beta" "beta should not exist yet"

info "create 'beta' via the CLI while the TUI runs — no key is sent to the TUI"
fleet_up beta >/dev/null

# The TUI must surface beta on its own, pushed over the server's Watch stream —
# no 'r' refresh. (150_tui_refresh covers the explicit-refresh path; this proves
# the live read-path the client/server split introduced.)
tui_wait_for "beta" 15

pass "TUI live-updates from the Watch stream (no manual refresh)"
