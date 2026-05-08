#!/usr/bin/env bash
# Description: Pressing `l` on a running instance shows its logs and Enter returns to the TUI.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha"  15
tui_wait_for "○ idle" 60

# ===========================================
# Move to alpha and press `l`. tea.ExecProcess suspends the TUI and runs
# a wrapper shell script that cats the creation log + container runtime
# logs, then waits on `read _` for an Enter keypress before returning.
# ===========================================
info "moving cursor to alpha"
tui_send j
sleep 0.3

info "pressing 'l' to view logs"
tui_send l

# The wrapper script always ends with this prompt — match on it as the
# canonical "logs view is up" signal.
info "waiting for press-Enter prompt"
tui_wait_for "--- Press Enter to return ---" 20

# Container runtime logs always include the entrypoint banner.
tui_assert_contains "Container started" "logs view should include container runtime output"
# The wrapper labels the runtime section explicitly.
tui_assert_contains "Container runtime logs" "logs view should label the runtime section"

# ===========================================
# Press Enter to dismiss; the TUI redraws and execDoneMsg triggers a
# reload + buildRows. The fleet row should be visible again.
# ===========================================
info "pressing Enter to dismiss logs view"
tui_send Enter
tui_wait_for_absent "--- Press Enter to return ---" 10
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

pass "TUI 'l' shows instance logs and dismisses on Enter"
