#!/usr/bin/env bash
# itest: k8s
# Description: TUI shift+j/k jumps between instance rows skipping session rows and wrapping
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test

# ===========================================
# Bring up two instances so there are two instance rows to jump between.
# ===========================================
info "bringing up first instance (alpha)"
fleet_up alpha

info "bringing up second instance (beta)"
fleet_up beta

tui_spawn
tui_wait_for "${FIXTURE_REPO_NAME} (2)" 30
tui_wait_for "alpha" 15
tui_wait_for "beta" 15
tui_wait_for "○ idle" 60

# ===========================================
# Helpers
# ===========================================
# The selected row is rendered with a literal "> " prefix inside the list
# box border. Find the one line on screen containing "> " and assert it
# also contains `needle`.
assert_cursor_on() {
  local needle="$1"; local msg="${2:-cursor not on expected row}"
  local screen cursor_line
  screen=$(tui_capture)
  cursor_line=$(printf '%s\n' "${screen}" | grep -F '> ' | head -1 || true)
  if ! printf '%s' "${cursor_line}" | grep -qF -- "${needle}"; then
    printf -- '--- screen ---\n%s\n--- end ---\n' "${screen}" >&2
    fail "${msg}: needle=[${needle}] cursor_line=[${cursor_line}]"
  fi
}

# ===========================================
# Seed cursor on alpha, then expand alpha so the row list contains a
# non-instance "+ new session" row between alpha and beta. This lets us
# prove that shift+j actually skips it, not just mimics plain j.
# ===========================================
info "moving cursor down from header to alpha"
tui_send j
sleep 0.3
assert_cursor_on "alpha" "cursor should be on alpha after 1× j"

info "expanding alpha so + new session row appears between alpha and beta"
tui_send Space
tui_wait_for "+ new session" 15
assert_cursor_on "alpha" "cursor should remain on alpha after expand"

# ===========================================
# shift+j / shift+k — capital-letter form
# ===========================================
info "shift+J from alpha should jump over + new session to beta"
tui_send J
sleep 0.3
assert_cursor_on "beta" "J should skip + new session and land on beta"

info "shift+J from beta should wrap past settings/header to alpha"
tui_send J
sleep 0.3
assert_cursor_on "alpha" "J should wrap from last instance to first"

info "shift+K from alpha should wrap backward past header/settings to beta"
tui_send K
sleep 0.3
assert_cursor_on "beta" "K should wrap from first instance to last"

info "shift+K from beta should skip + new session and land on alpha"
tui_send K
sleep 0.3
assert_cursor_on "alpha" "K should skip + new session and land on alpha"

# ===========================================
# shift+arrow form — proves the shift+Up / shift+Down keybinding path too
# ===========================================
info "shift+Down from alpha should jump over + new session to beta"
tui_send S-Down
sleep 0.3
assert_cursor_on "beta" "shift+Down should skip + new session and land on beta"

info "shift+Up from beta should jump back over + new session to alpha"
tui_send S-Up
sleep 0.3
assert_cursor_on "alpha" "shift+Up should skip + new session and land on alpha"

pass "TUI shift+j/k jumps between instance rows with wrapping"
