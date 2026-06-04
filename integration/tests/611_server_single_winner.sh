#!/usr/bin/env bash
# Description: concurrent cold commands collapse to exactly one daemon (single-winner flock).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true; }
itest_begin

server_count() { pgrep -fc "${FLEET_BIN} server" 2>/dev/null || true; }

setup_test
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false "" false

# Cold start.
pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
deadline=$(( $(date +%s) + $(_scale_timeout 5) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  if [ "$(server_count)" = "0" ]; then break; fi
  sleep 0.2
done
assert_equals "0" "$(server_count)" "no daemon should be running at cold start"

info "fire 5 commands concurrently from cold"
pids=""
for _ in 1 2 3 4 5; do
  "${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null 2>&1 &
  pids="${pids} $!"
done
rc=0
for p in ${pids}; do
  if ! wait "${p}"; then rc=1; fi
done
[ "${rc}" -eq 0 ] || fail "a concurrent cold command failed"

# The single-winner spawn lock must collapse the racing spawns into one daemon;
# losers wait for it and reuse it rather than each starting their own.
assert_equals "1" "$(server_count)" "racing spawns must yield exactly one daemon (single-winner flock)"

pass "single-winner daemon spawn under a race"
