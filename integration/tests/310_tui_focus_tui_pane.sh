#!/usr/bin/env bash
# itest: k8s
# Description: TUI installs a prefix + H binding that refocuses the fleet TUI pane
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha

tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

# ===========================================
# 1. Binding is installed in the prefix key table.
# ===========================================
info "asserting prefix + H is bound in the prefix table"
keys=$(tmux list-keys -T prefix 2>/dev/null || true)
h_line=$(printf '%s\n' "${keys}" | grep -E '(^| )H( |$)' | head -1 || true)
if [ -z "${h_line}" ]; then
  printf -- '--- list-keys prefix ---\n%s\n--- end ---\n' "${keys}" >&2
  fail "prefix + H binding missing from prefix table"
fi
info "binding: ${h_line}"
assert_contains "${h_line}" "select-pane" "prefix + H should run select-pane"
assert_contains "${h_line}" ":.0" "prefix + H should target pane 0 of the current window"

# ===========================================
# 2. Open a shell split so the active pane moves away from the TUI.
# ===========================================
info "moving cursor to alpha and opening a shell split"
tui_send j
sleep 0.3
tui_send Enter
tui_wait_for_pane 2 20
sleep 0.5

active_idx=$(tmux display-message -t "${TUI_SESSION}" -p '#{pane_index}')
info "active pane after opening split: index=${active_idx}"
if [ "${active_idx}" = "0" ]; then
  fail "expected the split to take focus, still on pane 0 (TUI)"
fi

# ===========================================
# 3. Invoking the same command the binding would run must refocus the
#    TUI pane. send-keys cannot trigger tmux bindings (it delivers keys
#    straight to the pane, bypassing the key table), so this is the
#    closest check we can make without a real terminal emulator driving
#    the tmux client.
# ===========================================
info "running the binding's command directly: select-pane -t :.0"
tmux select-pane -t "${TUI_SESSION}:.0"
sleep 0.3
active_idx=$(tmux display-message -t "${TUI_SESSION}" -p '#{pane_index}')
info "active pane after select-pane: index=${active_idx}"
assert_equals "0" "${active_idx}" "select-pane -t :.0 must focus the TUI pane"

# ===========================================
# 4. The split still exists — refocusing didn't destroy it.
# ===========================================
assert_equals "2" "$(tui_pane_count)" "refocus must not close the split pane"

pass "prefix + H binding installed and refocuses the TUI pane"
