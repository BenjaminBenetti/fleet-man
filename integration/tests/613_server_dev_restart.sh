#!/usr/bin/env bash
# Description: a dev-build client restarts a stale daemon when its binary is newer, and does NOT thrash otherwise.
set -euo pipefail

# Relies on a DEV build (version == ""), which CI produces (plain `go build`,
# no version ldflags). The reconcile logic: a dev client restarts a pre-existing
# local daemon when the client binary's mtime is newer than the daemon's
# started_at (from the Hello handshake), and leaves it alone when it isn't.

source "$(dirname "$0")/../common.sh"
itest_cleanup() { pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true; }
itest_begin

server_count() { pgrep -fc "${FLEET_BIN} server" 2>/dev/null || true; }
server_pid() { pgrep -f "${FLEET_BIN} server" 2>/dev/null | head -n1 || true; }
# wait_one_server [timeout] — wait until exactly one daemon runs, then print its pid.
wait_one_server() {
  local t; t=$(_scale_timeout "${1:-10}")
  local deadline=$(( $(date +%s) + t ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    if [ "$(server_count)" = "1" ]; then server_pid; return 0; fi
    sleep 0.25
  done
  return 1
}

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
pid1=$(wait_one_server 10) || fail "daemon did not start"
info "daemon pid1=${pid1}"

info "make the client binary newer than the running daemon"
sleep 1
touch "${FLEET_BIN}" || fail "cannot touch ${FLEET_BIN} — this test needs a dev build owned by the test user"

info "next command should transparently restart the now-stale daemon"
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null
pid2=$(wait_one_server 15) || fail "daemon did not stabilize after the restart"
info "daemon pid2=${pid2}"
[ "${pid2}" != "${pid1}" ] || fail "a stale daemon was NOT restarted (pid unchanged: ${pid1})"

info "an unchanged binary must NOT restart the daemon (no thrash)"
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null
pid3=$(wait_one_server 10) || fail "daemon disappeared on an unchanged-binary command"
info "daemon pid3=${pid3}"
assert_equals "${pid2}" "${pid3}" "unchanged binary must not restart the daemon (thrash)"

pass "dev-build staleness restart + no-thrash"
