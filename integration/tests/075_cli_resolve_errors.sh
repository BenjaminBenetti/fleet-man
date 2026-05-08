#!/usr/bin/env bash
# Description: CLI error paths — missing fleets, missing instances, and malformed targets fail with non-zero status.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test  # leaves ~/.fleet empty — no fleets registered in state

# Helper: run a command outside `set -e` and capture {stdout+stderr, rc}.
# Usage: run_capture <var_prefix> -- <fleet args...>
# Sets ${var_prefix}_OUT and ${var_prefix}_RC.
run_capture() {
  local prefix="$1"; shift
  [ "${1:-}" = "--" ] && shift
  local _out _rc
  set +e
  _out=$("${FLEET_BIN}" "$@" 2>&1)
  _rc=$?
  set -e
  printf -v "${prefix}_OUT" '%s' "${_out}"
  printf -v "${prefix}_RC" '%s' "${_rc}"
}

# ===========================================
# 1. Operations on a missing fleet must fail (non-zero) AND surface the
#    fleet name in the error so the user can debug.
# ===========================================
for verb in down destroy stop start logs; do
  case "${verb}" in
    destroy) target="ghost-fleet" ;;
    *)       target="ghost-fleet/missing-instance" ;;
  esac
  info "fleet ${verb} ${target} (fleet does not exist)"
  run_capture R -- "${verb}" "${target}"
  if [ "${R_RC}" -eq 0 ]; then
    fail "expected ${verb} on missing fleet to fail, got rc=0; out=[${R_OUT}]"
  fi
  assert_contains "${R_OUT}" "ghost-fleet" "${verb} error should mention fleet name"
done

# ===========================================
# 2. exec on a missing fleet — uses MinimumNArgs(2), so we must include a cmd.
# ===========================================
info "fleet exec ghost-fleet/missing -- echo hi (fleet does not exist)"
run_capture R -- exec ghost-fleet/missing -- echo hi
if [ "${R_RC}" -eq 0 ]; then
  fail "exec on missing fleet should fail, got rc=0; out=[${R_OUT}]"
fi
assert_contains "${R_OUT}" "ghost-fleet" "exec error should mention fleet name"

# ===========================================
# 3. Malformed target ("/foo" or "fleet/") — Resolve returns
#    "invalid target ... both fleet and instance names are required".
# ===========================================
info "fleet exec /foo -- echo hi (empty fleet)"
run_capture R -- exec "/foo" -- echo hi
if [ "${R_RC}" -eq 0 ]; then
  fail "exec with empty fleet name should fail, got rc=0; out=[${R_OUT}]"
fi
assert_contains "${R_OUT}" "invalid target" "empty-fleet target should be rejected as invalid"

info "fleet exec foo/ -- echo hi (empty instance)"
run_capture R -- exec "foo/" -- echo hi
if [ "${R_RC}" -eq 0 ]; then
  fail "exec with empty instance name should fail, got rc=0; out=[${R_OUT}]"
fi
assert_contains "${R_OUT}" "invalid target" "empty-instance target should be rejected as invalid"

# ===========================================
# 4. Operations on an existing fleet but a missing instance should
#    surface the instance name (not just the fleet).
# ===========================================
info "Bringing up itest-fleet/alpha to populate state"
fleet_up alpha

info "fleet down itest-fleet/no-such-instance (instance missing in real fleet)"
run_capture R -- down "${FIXTURE_REPO_NAME}/no-such-instance"
if [ "${R_RC}" -eq 0 ]; then
  fail "down on missing instance should fail, got rc=0; out=[${R_OUT}]"
fi
assert_contains "${R_OUT}" "no-such-instance" "down error should mention missing instance name"

info "fleet exec itest-fleet/no-such-instance -- echo hi"
run_capture R -- exec "${FIXTURE_REPO_NAME}/no-such-instance" -- echo hi
if [ "${R_RC}" -eq 0 ]; then
  fail "exec on missing instance should fail, got rc=0; out=[${R_OUT}]"
fi
assert_contains "${R_OUT}" "no-such-instance" "exec error should mention missing instance name"

# Sanity: the real instance still works after all the failed lookups —
# none of the error paths should have corrupted state.
real_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- sh -c "echo still-here")
assert_equals "still-here" "${real_out}" "valid instance should still be reachable after error-path probing"

pass "CLI resolve errors fail cleanly with descriptive messages"
