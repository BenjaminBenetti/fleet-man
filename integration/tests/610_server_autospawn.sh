#!/usr/bin/env bash
# Description: a cold `fleet` command auto-spawns the fleetd daemon (socket appears) and the next reuses it.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true; }
itest_begin

server_count() { pgrep -fc "${FLEET_BIN} server" 2>/dev/null || true; }
server_pid() { pgrep -f "${FLEET_BIN} server" 2>/dev/null | head -n1 || true; }

setup_test
# A seeded fleet record gives the read command real state to serve, proving the
# client actually reached the daemon (not merely that a process appeared).
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false "" false

# Cold start: kill any daemon left by an earlier test (it was bound to the now
# wiped ~/.fleet) and confirm none is running.
pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
deadline=$(( $(date +%s) + $(_scale_timeout 5) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  if [ "$(server_count)" = "0" ]; then break; fi
  sleep 0.2
done
assert_equals "0" "$(server_count)" "no daemon should be running at cold start"
assert_file_absent "${HOME}/.fleet/fleet.sock"

info "first command auto-spawns the daemon"
# A seeded but instance-less fleet makes `fleet ls` print only a header, so we
# assert the daemon SIDE-EFFECTS instead: a successful RPC round-trip (rc 0 from
# GetState) + the socket + exactly one daemon process. rc 0 proves the cold
# command actually reached the daemon, not merely that a process appeared.
set +e
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null 2>&1; rc=$?
set -e
[ "${rc}" -eq 0 ] || fail "cold command did not complete a successful RPC round-trip (rc=${rc})"
assert_file_exists "${HOME}/.fleet/fleet.sock"
assert_equals "1" "$(server_count)" "exactly one daemon should run after auto-spawn"
pid1=$(server_pid)
info "daemon pid=${pid1}"

info "second command reuses the same daemon"
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null
assert_equals "1" "$(server_count)" "second command must not spawn a second daemon"
assert_equals "${pid1}" "$(server_pid)" "second command must reuse the same daemon pid"

pass "daemon auto-spawn + reuse"
