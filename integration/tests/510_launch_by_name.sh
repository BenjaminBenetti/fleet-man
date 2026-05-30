#!/usr/bin/env bash
# Description: `fleet launch <name>` resolves names (unknown vs unique prefix)
# and reports a missing host control socket instead of crashing.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

# Launch fixture: Links "Grafana" + "Project Docs", App "Webapp".
setup_launch_test

info "fleet up alpha (launch fixture)"
fleet_up alpha

# 1. An unknown name fails fast at resolution — before any host dial —
#    and points the user at `fleet launch list`.
info "fleet exec alpha -- fleet launch nope-not-a-thing (expect resolve error)"
set +e
unknown_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" \
  -- fleet launch nope-not-a-thing 2>&1)
unknown_rc=$?
set -e
info "output: ${unknown_out}"
[ "${unknown_rc}" -ne 0 ] || fail "unknown name unexpectedly succeeded"
assert_contains "${unknown_out}" "no link or app matching" \
  "unknown name did not produce the expected resolve error"

# 2. A unique case-insensitive prefix ("graf" -> "Grafana") resolves, then
#    tries to drive the host browser over the control socket. No host fleet
#    TUI runs in CI, so the socket file never appears in the mounted control
#    dir and the dial fails with a clear "not connected to host fleet"
#    message — proving the resolve + socket-dial path ran end to end.
info "fleet exec alpha -- fleet launch graf (expect not-connected error)"
set +e
prefix_out=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" \
  -- fleet launch graf 2>&1)
prefix_rc=$?
set -e
info "output: ${prefix_out}"
[ "${prefix_rc}" -ne 0 ] || fail "launch graf unexpectedly succeeded without a host TUI"
assert_contains "${prefix_out}" "not connected to host fleet" \
  "prefix match did not reach the host-socket dial"
# It must NOT be a resolution failure — the prefix is unambiguous.
assert_not_contains "${prefix_out}" "no link or app matching" \
  "unique prefix 'graf' failed to resolve to Grafana"

pass "launch by name"
