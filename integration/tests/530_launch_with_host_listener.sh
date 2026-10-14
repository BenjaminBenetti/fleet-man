#!/usr/bin/env bash
# itest: k8s
# Description: with a host TUI attached, in-container `fleet launch <name>` succeeds (the control socket is open).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

# Launch fixture (Grafana link etc.), one running instance.
setup_launch_test
fleet_up alpha

inst="${FIXTURE_REPO_NAME}/alpha"

# With NO TUI attached the subscriber gate keeps the control socket closed —
# assert that first, so the socket appearing below is causally attributable to
# the TUI attaching (not a pre-existing socket). This is the positive complement
# to 510, which proves the no-TUI case where `fleet launch` reports
# "not connected to host fleet".
if itest_control_test "${inst}" -e fleet.sock; then
  fail "the control socket fleet.sock is open with no TUI attached"
fi

# Attaching a TUI makes it a Watch subscriber, so the server opens the
# per-instance control socket within one controlReg tick.
tui_spawn
tui_wait_for "alpha" 15

info "waiting for the server to open the control socket (TUI now attached)"
deadline=$(( $(date +%s) + $(_scale_timeout 20) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  if itest_control_test "${inst}" -S fleet.sock; then break; fi
  sleep 0.25
done
itest_control_test "${inst}" -S fleet.sock || fail "the control socket fleet.sock never opened"

info "fleet launch graf inside the instance (host listener present)"
set +e
out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- fleet launch graf 2>&1)
rc=$?
set -e
info "output: ${out}"
[ "${rc}" -eq 0 ] || fail "launch graf should succeed with a host TUI attached (rc=${rc}): ${out}"
assert_not_contains "${out}" "not connected to host fleet" \
  "control socket present but launch reported not-connected"

pass "launch by name reaches an attached host listener"
