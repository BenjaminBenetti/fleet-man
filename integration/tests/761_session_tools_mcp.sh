#!/usr/bin/env bash
# itest: k8s
# Description: session tools over MCP — fleet_exec returns a command's stdout, stderr and exit code; fleet_session_spawn / fleet_session_exec / fleet_session_read (with scrollback) / fleet_session_list drive a tmux session in the instance, sharing it with the CLI session tools; a stopped instance is refused with a tool error.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"
target="\"fleet\":\"${FIXTURE_REPO_NAME}\",\"instance\":\"alpha\""

# mcp <tool> <extra-args-json> [<field>] — itest_mcp_call against alpha.
mcp() {
  local extra="${2:-}"
  itest_mcp_call "$1" "{${target}${extra:+,${extra}}}" "${3:-}"
}

# wait_mcp_session_has <session> <needle> [scrollback] — poll fleet_session_read
# until the screen (plus <scrollback> lines of history) shows <needle>.
wait_mcp_session_has() {
  local session="$1" needle="$2" scrollback="${3:-0}" out="" deadline
  deadline=$(( $(date +%s) + $(_scale_timeout 20) ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    out=$(mcp fleet_session_read "\"session\":\"${session}\",\"scrollback\":${scrollback}" \
      structuredContent.content || true)
    if printf '%s' "${out}" | grep -qF -- "${needle}"; then
      return 0
    fi
    sleep 0.5
  done
  printf -- '--- fleet_session_read %s ---\n%s\n--- end ---\n' "${session}" "${out}" >&2
  return 1
}

# ===========================================
# fleet_exec: one-shot command, output and exit code as data.
# ===========================================
info "fleet_exec reports stdout, stderr and a non-zero exit as data"
res=$(mcp fleet_exec '"command":["sh","-c","printf OUT_$((6*7)); printf ERR_7 >&2; exit 7"]') \
  || fail "fleet_exec did not answer"
assert_equals "OUT_42" "$(printf '%s' "${res}" | itest_json_field structuredContent.stdout)" "fleet_exec stdout"
assert_equals "ERR_7" "$(printf '%s' "${res}" | itest_json_field structuredContent.stderr)" "fleet_exec stderr"
assert_equals "7" "$(printf '%s' "${res}" | itest_json_field structuredContent.exit_code)" "fleet_exec exit_code"
assert_not_contains "$(printf '%s' "${res}" | itest_json_field isError)" "true" "a non-zero exit is data, not a tool error"

# argv, not a shell string: the words reach the command untouched.
out=$(mcp fleet_exec '"command":["printf","%s|","a b","$HOME"]' structuredContent.stdout)
assert_equals 'a b|$HOME|' "${out}" "fleet_exec should pass argv words through unsplit and unexpanded"

# The command runs in the instance's workspace checkout.
out=$(mcp fleet_exec '"command":["cat","README.md"]' structuredContent.stdout)
assert_contains "${out}" "fleet-man integration fixture" "fleet_exec should run in the workspace checkout"

# ===========================================
# fleet_session_spawn / exec / read / list.
# ===========================================
info "fleet_session_list on an instance with no sessions is an empty list"
out=$(mcp fleet_session_list "" structuredContent.sessions)
assert_equals "[]" "${out}" "fleet_session_list with no tmux sessions"

info "fleet_session_spawn creates alpha~mcp"
out=$(mcp fleet_session_spawn '"session":"mcp"' structuredContent.message)
assert_contains "${out}" "alpha~mcp" "fleet_session_spawn should name the canonical <instance>~<name> session"

out=$(mcp fleet_session_spawn '"session":"mcp"' isError)
assert_equals "true" "${out}" "spawning an existing session name should be a tool error"

info "fleet_session_exec runs a command in the session's shell"
out=$(mcp fleet_session_exec '"session":"mcp","command":"echo MCP_$((6*7))"' structuredContent.message)
assert_contains "${out}" "alpha~mcp" "fleet_session_exec should report the session it typed into"
wait_mcp_session_has mcp "MCP_42" || fail "fleet_session_read never showed the command's output"

info "fleet_session_read scrollback reaches lines above the screen"
mcp fleet_session_exec "\"session\":\"mcp\",\"command\":\"seq -f 'M%03g' 1 150; echo SEQ_DONE_\$((1+1))\"" >/dev/null \
  || fail "fleet_session_exec (seq) did not answer"
wait_mcp_session_has mcp "SEQ_DONE_2" || fail "the seq output never reached the screen"
visible=$(mcp fleet_session_read '"session":"mcp"' structuredContent.content)
assert_contains "${visible}" "M150" "the visible screen should end with the last seq line"
assert_not_contains "${visible}" "M001" "scrollback 0 should print only the visible screen"
some=$(mcp fleet_session_read '"session":"mcp","scrollback":60' structuredContent.content)
assert_contains "${some}" "M100" "scrollback 60 should reach 60 lines above the screen"
assert_not_contains "${some}" "M001" "scrollback 60 should stop 60 lines above the screen"
all=$(mcp fleet_session_read '"session":"mcp","scrollback":-1' structuredContent.content)
assert_contains "${all}" "M001" "a negative scrollback should print the whole history"

# The CLI session tools and the MCP tools work on the same sessions.
info "a session spawned by the CLI shows in fleet_session_list and reads over MCP"
"${FLEET_BIN}" spawn-session "${inst}" cli >/dev/null || fail "spawn-session (CLI) failed"
"${FLEET_BIN}" exec-in-session "${inst}" cli 'echo FROM_CLI_$((5*5))' >/dev/null || fail "exec-in-session (CLI) failed"
wait_mcp_session_has cli "FROM_CLI_25" || fail "fleet_session_read could not read the CLI-spawned session"
out=$("${FLEET_BIN}" read-session "${inst}" mcp) || fail "read-session (CLI) of the MCP-spawned session failed"
assert_contains "${out}" "M150" "read-session should read the session fleet_session_spawn made"

sessions=$(mcp fleet_session_list "" structuredContent.sessions)
printf '%s\n' "${sessions}"
assert_contains "${sessions}" '"session":"alpha~mcp"' "fleet_session_list should list the MCP-spawned session"
assert_contains "${sessions}" '"session":"alpha~cli"' "fleet_session_list should list the CLI-spawned session"
assert_contains "${sessions}" '"windows":1' "fleet_session_list should report each session's window count"
assert_contains "${sessions}" '"attached":false' "detached sessions should be listed as not attached"
assert_contains "${sessions}" '"created_at":"' "fleet_session_list should report when each session was created"

# ===========================================
# Errors are tool errors with a readable message.
# ===========================================
out=$(mcp fleet_session_read '"session":"no-such-session"' isError)
assert_equals "true" "${out}" "reading a missing session should be a tool error"
out=$(itest_mcp_call fleet_exec "{\"fleet\":\"${FIXTURE_REPO_NAME}\",\"instance\":\"ghost\",\"command\":[\"true\"]}" isError)
assert_equals "true" "${out}" "fleet_exec on a missing instance should be a tool error"

info "the exec and session tools refuse a stopped instance"
"${FLEET_BIN}" stop "${inst}" >/dev/null
for tool in fleet_exec fleet_session_list fleet_session_read; do
  case "${tool}" in
    fleet_exec) extra='"command":["true"]' ;;
    fleet_session_read) extra='"session":"mcp"' ;;
    *) extra="" ;;
  esac
  res=$(mcp "${tool}" "${extra}") || fail "${tool} did not answer on a stopped instance"
  assert_equals "true" "$(printf '%s' "${res}" | itest_json_field isError)" "${tool} on a stopped instance should be a tool error"
  assert_contains "$(printf '%s' "${res}" | itest_json_field content.0.text)" "is not running" \
    "${tool} on a stopped instance should say it is not running"
done

pass "fleet_exec and fleet_session_spawn/exec/read/list over MCP, shared with the CLI session tools"
