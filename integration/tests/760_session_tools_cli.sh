#!/usr/bin/env bash
# itest: k8s
# Description: session tools CLI round trip — spawn-session creates a detached tmux session in the instance, exec-in-session runs a command in its shell, and read-session prints the screen, with --scrollback reaching lines above it (N lines, then the full history).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

# wait_session_has <session> <needle> [scrollback] — poll read-session until
# the screen (plus <scrollback> lines of history) shows <needle>. exec-in-session
# only types the command, so its output lands a moment later.
wait_session_has() {
  local session="$1" needle="$2" scrollback="${3:-0}" out="" deadline
  deadline=$(( $(date +%s) + $(_scale_timeout 20) ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    out=$("${FLEET_BIN}" read-session "${inst}" "${session}" --scrollback "${scrollback}" 2>&1 || true)
    if printf '%s' "${out}" | grep -qF -- "${needle}"; then
      return 0
    fi
    sleep 0.5
  done
  printf -- '--- read-session %s ---\n%s\n--- end ---\n' "${session}" "${out}" >&2
  return 1
}

info "spawn-session alpha work"
out=$("${FLEET_BIN}" spawn-session "${inst}" work 2>&1) || fail "spawn-session failed: ${out}"
assert_contains "${out}" "alpha~work" "spawn-session should name the canonical <instance>~<name> session"

# The session is a real tmux session in the instance, under the TUI's group
# naming, as the session user (the same tmux server fleet exec reaches).
sessions=$("${FLEET_BIN}" exec "${inst}" -- tmux list-sessions -F '#{session_name}' 2>&1) \
  || fail "tmux list-sessions in the instance failed: ${sessions}"
printf '%s\n' "${sessions}" | grep -qxF 'alpha~work' \
  || fail "spawn-session left no alpha~work session in the instance: ${sessions}"

# A second spawn of the same name is refused (tmux: duplicate session).
if "${FLEET_BIN}" spawn-session "${inst}" work >/dev/null 2>&1; then
  fail "spawn-session of an existing session name should fail"
fi

# The arithmetic only expands when the shell runs the line, so CLI_42 on the
# screen proves exec-in-session executed the command, not just typed it.
info "exec-in-session runs a command in the session's shell"
out=$("${FLEET_BIN}" exec-in-session "${inst}" work 'echo CLI_$((6*7))' 2>&1) \
  || fail "exec-in-session failed: ${out}"
wait_session_has work "CLI_42" || fail "read-session never showed the command's output"

# The short name and the canonical name address the same session.
out=$("${FLEET_BIN}" read-session "${inst}" 'alpha~work' 2>&1) || fail "read-session by canonical name failed: ${out}"
assert_contains "${out}" "CLI_42" "read-session by the canonical name should read the same session"

# --- scrollback ---
# 150 numbered lines overflow the detached session's 24-row screen, so the
# first ones are only in tmux's history. Zero-padded so L001 is no substring
# of another line.
info "exec-in-session prints 150 lines; read-session --scrollback reaches the history"
"${FLEET_BIN}" exec-in-session "${inst}" work "seq -f 'L%03g' 1 150; echo SEQ_DONE_\$((1+1))" >/dev/null \
  || fail "exec-in-session (seq) failed"
wait_session_has work "SEQ_DONE_2" || fail "the seq output never reached the screen"

visible=$("${FLEET_BIN}" read-session "${inst}" work)
assert_contains "${visible}" "L150" "the visible screen should end with the last seq line"
assert_not_contains "${visible}" "L001" "without --scrollback only the visible screen is printed"

some=$("${FLEET_BIN}" read-session "${inst}" work --scrollback 60)
assert_contains "${some}" "L100" "--scrollback 60 should reach 60 lines above the screen"
assert_not_contains "${some}" "L001" "--scrollback 60 should stop 60 lines above the screen"

all=$("${FLEET_BIN}" read-session "${inst}" work -s -1)
assert_contains "${all}" "L001" "--scrollback -1 should print the whole history"
assert_contains "${all}" "CLI_42" "the full history should still hold the first command's output"

# --- errors ---
if "${FLEET_BIN}" read-session "${inst}" no-such-session >/dev/null 2>&1; then
  fail "read-session of a missing session should fail"
fi
if "${FLEET_BIN}" exec-in-session "${inst}" no-such-session 'true' >/dev/null 2>&1; then
  fail "exec-in-session into a missing session should fail"
fi

info "the session tools refuse a stopped instance"
"${FLEET_BIN}" stop "${inst}" >/dev/null
out=$("${FLEET_BIN}" read-session "${inst}" work 2>&1) && fail "read-session on a stopped instance should fail"
assert_contains "${out}" "not running" "read-session on a stopped instance should say it is not running"

pass "spawn-session / exec-in-session / read-session round trip, with --scrollback into the history"
