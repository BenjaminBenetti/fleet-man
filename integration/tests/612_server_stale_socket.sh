#!/usr/bin/env bash
# itest: no-docker
# Description: a hard-killed daemon leaves a stale socket; the next command clears it and respawns cleanly.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true; }
itest_begin


setup_test
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false "" false

pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
deadline=$(( $(date +%s) + $(_scale_timeout 5) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  if [ "$(server_count)" = "0" ]; then break; fi
  sleep 0.2
done

info "spawn the daemon"
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null
assert_file_exists "${HOME}/.fleet/fleet.sock"
assert_equals "1" "$(server_count)" "daemon should be running"

info "SIGKILL the daemon — it cannot unlink its socket, so a stale file remains"
pkill -9 -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
deadline=$(( $(date +%s) + $(_scale_timeout 5) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  if [ "$(server_count)" = "0" ]; then break; fi
  sleep 0.2
done
assert_equals "0" "$(server_count)" "daemon should be dead after SIGKILL"
assert_file_exists "${HOME}/.fleet/fleet.sock"

info "next command must clear the stale socket and respawn"
# Assert recovery via daemon side-effects (the seeded fleet is instance-less, so
# `fleet ls` prints only a header): a successful RPC round-trip + a fresh socket
# + exactly one daemon.
set +e
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null 2>&1; rc=$?
set -e
[ "${rc}" -eq 0 ] || fail "command failed to recover from a stale socket (rc=${rc})"
assert_file_exists "${HOME}/.fleet/fleet.sock"
assert_equals "1" "$(server_count)" "exactly one daemon after stale-socket recovery"

pass "stale-socket recovery"
