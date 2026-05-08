#!/usr/bin/env bash
# Shared helpers for fleet-man integration tests.
#
# Each test file sources this, calls `setup_test`, runs assertions, and
# exits 0 on pass. `run.sh` invokes teardown between tests so state does
# not bleed across runs.

set -euo pipefail

# Colors (only when stdout is a tty)
if [ -t 1 ]; then
  C_RED='\033[0;31m'
  C_GREEN='\033[0;32m'
  C_YELLOW='\033[0;33m'
  C_RESET='\033[0m'
else
  C_RED=''
  C_GREEN=''
  C_YELLOW=''
  C_RESET=''
fi

# Paths resolved relative to this file's directory.
INTEGRATION_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${INTEGRATION_DIR}/.." && pwd)"
FIXTURE_SRC="${INTEGRATION_DIR}/fixture"

# Test scratch dir created fresh per test.
TEST_SCRATCH_DIR="${TEST_SCRATCH_DIR:-/tmp/fleet-man-itest}"

# The fleet binary under test. run.sh builds it and exports FLEET_BIN.
FLEET_BIN="${FLEET_BIN:-fleet}"

# Fleet name is derived from the repo URL basename. We clone the fixture
# into a dir named "itest-fleet" so resolve.go sees fleet "itest-fleet".
FIXTURE_REPO_NAME="itest-fleet"
FIXTURE_REPO_DIR="${TEST_SCRATCH_DIR}/${FIXTURE_REPO_NAME}"
FIXTURE_REPO_URL="file://${FIXTURE_REPO_DIR}"

# Default tmux session name used by TUI tests. Overridable per-test.
TUI_SESSION="${TUI_SESSION:-fleetman-itest}"
TUI_WIDTH="${TUI_WIDTH:-140}"
TUI_HEIGHT="${TUI_HEIGHT:-40}"

# === Logging / result emission ===
#
# Each test is responsible for emitting its OWN result row — not the
# runner. `itest_begin` is called at the top of every test: it records
# the start time and installs an EXIT trap so a row is always emitted,
# even if the script crashes or `set -e` trips.
#
# `pass` / `fail` both print and emit the row explicitly. The EXIT trap
# only emits if neither was reached (catch-all for unexpected bash exits).
# The row format written to ${FLEET_ITEST_RESULTS} is:
#   <name>|<pass|fail>|<duration_s>|<description>
# run.sh aggregates this file into the final table. No central list
# needs editing when a new test is added.

info()  { printf "%b[info]%b  %s\n" "${C_YELLOW}" "${C_RESET}" "$*"; }

pass()  {
  printf "%b[pass]%b  %s\n" "${C_GREEN}"  "${C_RESET}" "$*"
  _itest_emit pass
  exit 0
}

fail()  {
  printf "%b[fail]%b  %s\n" "${C_RED}" "${C_RESET}" "$*" >&2
  _itest_emit fail
  exit 1
}

# itest_cleanup is a hook tests can override to run cleanup (e.g. kill a
# tmux session) before the EXIT trap emits the result row. Default: no-op.
itest_cleanup() { :; }

# itest_begin: call once per test, right after sourcing common.sh and
# before the test body. Records metadata and installs the result-emit
# EXIT trap.
itest_begin() {
  TEST_NAME="$(basename "$0" .sh)"
  TEST_DESCRIPTION="$(grep -m1 -E '^# Description:' "$0" 2>/dev/null \
    | sed -E 's/^# Description: *//')"
  if [ -z "${TEST_DESCRIPTION}" ]; then
    TEST_DESCRIPTION="(no description)"
  fi
  TEST_START="$(date +%s)"
  TEST_EMITTED=""
  trap _itest_exit_handler EXIT
}

# Internal: runs on EXIT. Calls itest_cleanup, then emits a row if the
# test did not already do so via pass/fail.
_itest_exit_handler() {
  local rc=$?
  itest_cleanup || true
  if [ -z "${TEST_EMITTED:-}" ]; then
    local status="fail"
    [ "${rc}" -eq 0 ] && status="pass"
    _itest_emit "${status}"
  fi
  # Preserve the original exit code.
  exit "${rc}"
}

# Internal: append one row to the shared results file. Safe to call
# outside a runner (just becomes a no-op when FLEET_ITEST_RESULTS is unset).
_itest_emit() {
  TEST_EMITTED=1
  local status="$1"
  local dur=0
  if [ -n "${TEST_START:-}" ]; then
    dur=$(( $(date +%s) - TEST_START ))
  fi
  if [ -n "${FLEET_ITEST_RESULTS:-}" ]; then
    printf '%s|%s|%s|%s\n' \
      "${TEST_NAME:-unknown}" \
      "${status}" \
      "${dur}" \
      "${TEST_DESCRIPTION:-(no description)}" \
      >> "${FLEET_ITEST_RESULTS}"
  fi
}

# === Assertions ===

# assert_equals <expected> <actual> <message>
assert_equals() {
  local expected="$1"; local actual="$2"; local msg="${3:-values not equal}"
  if [ "$expected" != "$actual" ]; then
    fail "${msg}: expected=[${expected}] actual=[${actual}]"
  fi
}

# assert_contains <haystack> <needle> <message>
assert_contains() {
  local haystack="$1"; local needle="$2"; local msg="${3:-substring not found}"
  if ! printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
    fail "${msg}: needle=[${needle}] haystack=[${haystack}]"
  fi
}

# assert_not_contains <haystack> <needle> <message>
assert_not_contains() {
  local haystack="$1"; local needle="$2"; local msg="${3:-substring unexpectedly found}"
  if printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
    fail "${msg}: needle=[${needle}] haystack=[${haystack}]"
  fi
}

# assert_file_exists <path>
assert_file_exists() {
  [ -e "$1" ] || fail "expected path to exist: $1"
}

# assert_file_absent <path>
assert_file_absent() {
  [ ! -e "$1" ] || fail "expected path to be absent: $1"
}

# === Setup / teardown ===

# setup_test prepares a clean environment:
#   1. removes ~/.fleet
#   2. recreates the fixture git repo at $FIXTURE_REPO_DIR
# Called at the top of each test.
setup_test() {
  # 1. Wipe fleet state. Leave an empty ~/.fleet behind — its presence
  #    suppresses the first-launch deps-check splash, which is not what
  #    these tests are trying to exercise and would otherwise block key
  #    input to the fleet list page.
  rm -rf "${HOME}/.fleet"
  mkdir -p "${HOME}/.fleet"

  # 2. Re-create the fixture repo from the committed fixture template.
  rm -rf "${TEST_SCRATCH_DIR}"
  mkdir -p "${FIXTURE_REPO_DIR}"
  cp -r "${FIXTURE_SRC}/." "${FIXTURE_REPO_DIR}/"

  (
    cd "${FIXTURE_REPO_DIR}"
    git init -q -b main
    git config user.email "itest@fleet-man.local"
    git config user.name  "Fleet Integration Test"
    git add -A
    git commit -q -m "fixture: initial commit"
  )
}

# teardown_test best-effort cleans up any containers spawned by a test.
# It tears down every fleet in state, then wipes state files. Called by
# run.sh after each test whether it passed or failed.
teardown_test() {
  # Kill any TUI tmux session left over from a TUI test. Safe if there
  # is none. Done first so nothing else is holding the state file open.
  if command -v tmux >/dev/null 2>&1; then
    tmux kill-session -t "${TUI_SESSION}" >/dev/null 2>&1 || true
  fi

  # Tear down containers. Best effort — a test that never reached
  # "up" will have no fleets; a test that left state in a weird place
  # is recovered by the fallback docker sweep below.
  if [ -f "${HOME}/.fleet/state.json" ]; then
    # Iterate fleet names out of state.json without jq (not guaranteed on
    # the runner). Grep the top-level keys under "fleets".
    local fleets
    fleets=$(grep -oE '"[a-zA-Z0-9._-]+":\s*\{' "${HOME}/.fleet/state.json" 2>/dev/null \
      | grep -v '"fleets"' \
      | sed -E 's/"([^"]+)".*/\1/' \
      | sort -u || true)
    for f in ${fleets}; do
      "${FLEET_BIN}" destroy "${f}" >/dev/null 2>&1 || true
    done
  fi

  # Fallback: remove any devcontainer created from the fixture workspace.
  # The devcontainer CLI labels containers with the workspace folder path.
  if command -v docker >/dev/null 2>&1; then
    local stale
    stale=$(docker ps -aq --filter "label=devcontainer.local_folder=${HOME}/.fleet/workspaces" 2>/dev/null || true)
    if [ -n "${stale}" ]; then
      docker rm -f ${stale} >/dev/null 2>&1 || true
    fi
  fi

  rm -rf "${HOME}/.fleet"
  rm -rf "${TEST_SCRATCH_DIR}"
}

# fleet_up spawns an instance against the fixture repo. Uses --repo so
# the test does not depend on cwd being a git repo. Streams output to
# the caller's stdout/stderr.
fleet_up() {
  local name="$1"
  "${FLEET_BIN}" up "${name}" --repo "${FIXTURE_REPO_URL}"
}

# === TUI helpers ===
#
# The fleet TUI is a bubbletea app. Tests drive it the same way a human
# would: launch `fleet` inside a detached tmux session of fixed size,
# then send keys + capture the rendered screen.

# _scale_timeout <seconds>
# Multiply a wait-budget by FLEET_TUI_WAIT_SCALE (default 1, integer).
# Slow runners (e.g. WSL on a Windows host) export a higher value so the
# polling helpers below give the TUI/dockerd enough headroom without us
# editing every test.
_scale_timeout() {
  local raw="$1"
  local scale="${FLEET_TUI_WAIT_SCALE:-1}"
  printf '%s' "$(( raw * scale ))"
}

# tui_spawn [session] [args...]
# Start the TUI inside a detached tmux session. Any extra arguments are
# passed through to the fleet binary. The TMUX env var is implicitly set
# by running inside tmux, which the TUI uses to enable split pane mode.
tui_spawn() {
  local session="${1:-${TUI_SESSION}}"
  shift || true
  tmux kill-session -t "${session}" >/dev/null 2>&1 || true

  # Force TERM to something tmux+lipgloss render predictably on CI.
  tmux new-session -d -s "${session}" -x "${TUI_WIDTH}" -y "${TUI_HEIGHT}" \
    "env TERM=xterm-256color ${FLEET_BIN} $*"
}

# tui_send_text <text> [session]
# Type a string literally into the TUI (safe for URLs / names that
# contain tmux-reserved key words).
tui_send_text() {
  local text="$1"
  local session="${2:-${TUI_SESSION}}"
  tmux send-keys -t "${session}" -l "${text}"
}

# tui_send_pane <pane_index> <key> [session]
# Send a key to a specific pane by its visible index (0 = TUI, 1 = first
# split, 2 = second split, ...). Needed once a split pane is open and
# we want to talk to the shell rather than the TUI.
tui_send_pane() {
  local pane="$1"; local key="$2"; local session="${3:-${TUI_SESSION}}"
  tmux send-keys -t "${session}:.${pane}" "${key}"
}

# tui_send_pane_text <pane_index> <text> [session]
tui_send_pane_text() {
  local pane="$1"; local text="$2"; local session="${3:-${TUI_SESSION}}"
  tmux send-keys -t "${session}:.${pane}" -l "${text}"
}

# tui_capture_pane <pane_index> [session]
# Capture the visible contents of a specific pane, scrolled from the
# history so we see output that has already scrolled off-screen.
tui_capture_pane() {
  local pane="$1"; local session="${2:-${TUI_SESSION}}"
  tmux capture-pane -t "${session}:.${pane}" -p -S -200
}

# tui_pane_count [session] — number of panes in the active window.
tui_pane_count() {
  local session="${1:-${TUI_SESSION}}"
  tmux list-panes -t "${session}" 2>/dev/null | wc -l
}

# tui_wait_for_pane <expected_count> [timeout_s] [session]
# Wait until the tmux window has `expected_count` panes (e.g. 2 after a
# split opens, 1 after it's closed).
tui_wait_for_pane() {
  local expected="$1"; local timeout
  timeout=$(_scale_timeout "${2:-10}")
  local session="${3:-${TUI_SESSION}}"
  local deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    [ "$(tui_pane_count "${session}")" -eq "${expected}" ] && return 0
    sleep 0.25
  done
  printf '%b[fail]%b tui_wait_for_pane timed out — expected %s panes, have %s\n' \
    "${C_RED}" "${C_RESET}" "${expected}" "$(tui_pane_count "${session}")" >&2
  return 1
}

# tui_wait_for_in_pane <pane_index> <needle> [timeout_s] [session]
# Poll a specific pane's capture for a substring.
tui_wait_for_in_pane() {
  local pane="$1"; local needle="$2"; local timeout
  timeout=$(_scale_timeout "${3:-10}")
  local session="${4:-${TUI_SESSION}}"
  local deadline=$(( $(date +%s) + timeout ))
  local screen=""
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    screen=$(tui_capture_pane "${pane}" "${session}" || true)
    if printf '%s' "${screen}" | grep -qF -- "${needle}"; then
      return 0
    fi
    sleep 0.25
  done
  printf '%b[fail]%b timed out after %ss waiting in pane %s for: %s\n' \
    "${C_RED}" "${C_RESET}" "${timeout}" "${pane}" "${needle}" >&2
  printf -- '--- pane %s ---\n%s\n--- end ---\n' "${pane}" "${screen}" >&2
  return 1
}

# tui_capture [session]
# Print the current visible screen of the tmux session to stdout.
tui_capture() {
  local session="${1:-${TUI_SESSION}}"
  tmux capture-pane -t "${session}" -p
}

# tui_send <key> [session]
# Send a single key / chord to the session. Use tmux key names: j, k,
# Enter, Escape, Space, C-c, etc.
tui_send() {
  local key="$1"
  local session="${2:-${TUI_SESSION}}"
  tmux send-keys -t "${session}" "${key}"
}

# tui_wait_for <needle> [timeout_s] [session]
# Poll capture-pane for up to timeout seconds waiting for `needle` (plain
# substring) to appear. Default timeout 10s. Returns 0 on match, 1 on
# timeout. On timeout also dumps the final screen to stderr.
tui_wait_for() {
  local needle="$1"
  local timeout
  timeout=$(_scale_timeout "${2:-10}")
  local session="${3:-${TUI_SESSION}}"
  local deadline=$(( $(date +%s) + timeout ))
  local screen=""
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    screen=$(tui_capture "${session}" || true)
    if printf '%s' "${screen}" | grep -qF -- "${needle}"; then
      return 0
    fi
    sleep 0.25
  done
  printf '%b[fail]%b tui_wait_for timed out after %ss waiting for: %s\n' \
    "${C_RED}" "${C_RESET}" "${timeout}" "${needle}" >&2
  printf -- '--- final screen ---\n%s\n--- end screen ---\n' "${screen}" >&2
  return 1
}

# tui_wait_for_absent <needle> [timeout_s] [session]
# Inverse of tui_wait_for — poll until the needle disappears from screen.
tui_wait_for_absent() {
  local needle="$1"
  local timeout
  timeout=$(_scale_timeout "${2:-10}")
  local session="${3:-${TUI_SESSION}}"
  local deadline=$(( $(date +%s) + timeout ))
  local screen=""
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    screen=$(tui_capture "${session}" || true)
    if ! printf '%s' "${screen}" | grep -qF -- "${needle}"; then
      return 0
    fi
    sleep 0.25
  done
  printf '%b[fail]%b tui_wait_for_absent timed out after %ss waiting for: %s\n' \
    "${C_RED}" "${C_RESET}" "${timeout}" "${needle}" >&2
  printf -- '--- final screen ---\n%s\n--- end screen ---\n' "${screen}" >&2
  return 1
}

# tui_kill [session]
# Kill the tmux session if it is still around. Safe to call multiple times.
tui_kill() {
  local session="${1:-${TUI_SESSION}}"
  tmux kill-session -t "${session}" >/dev/null 2>&1 || true
}

# tui_assert_contains <needle> [message]
# Fails the test if the current screen does NOT contain needle.
tui_assert_contains() {
  local needle="$1"; local msg="${2:-screen missing expected text}"
  local screen
  screen=$(tui_capture)
  if ! printf '%s' "${screen}" | grep -qF -- "${needle}"; then
    printf -- '--- screen ---\n%s\n--- end ---\n' "${screen}" >&2
    fail "${msg}: needle=[${needle}]"
  fi
}

# tui_assert_not_contains <needle> [message]
tui_assert_not_contains() {
  local needle="$1"; local msg="${2:-screen unexpectedly contains text}"
  local screen
  screen=$(tui_capture)
  if printf '%s' "${screen}" | grep -qF -- "${needle}"; then
    printf -- '--- screen ---\n%s\n--- end ---\n' "${screen}" >&2
    fail "${msg}: needle=[${needle}]"
  fi
}
