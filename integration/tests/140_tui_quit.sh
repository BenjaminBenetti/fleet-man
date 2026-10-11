#!/usr/bin/env bash
# itest: k8s
# Description: Pressing `q` exits the TUI and the tmux session dies.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha" 15

info "Quit with q"
tui_send q

# The fleet binary exits → the tmux session terminates. Poll for the
# session to be gone. `has-session` returns non-zero once it is killed.
deadline=$(( $(date +%s) + 10 ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  if ! tmux has-session -t "${TUI_SESSION}" 2>/dev/null; then
    pass "TUI quit via q"
    exit 0
  fi
  sleep 0.25
done
fail "tmux session ${TUI_SESSION} still alive after 'q'"
