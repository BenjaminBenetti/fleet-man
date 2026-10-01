#!/usr/bin/env bash
# itest: no-docker
# Description: a daemon that is alive but answers nothing is killed and replaced by the next command.
set -euo pipefail

# The daemon holds its lifetime lock for as long as its process lives, so one
# that stops answering (wedged mid-shutdown, deadlocked, frozen) blocks every
# replacement: before the fix each command timed out until someone ran
# `kill -9`. SIGSTOP stands in for the wedge — the process keeps its lock and
# its socket and answers nothing.

source "$(dirname "$0")/../common.sh"
itest_cleanup() {
  pkill -CONT -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
}
itest_begin

server_pid() { pgrep -f "${FLEET_BIN} server" 2>/dev/null | head -n1 || true; }

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
assert_equals "1" "$(server_count)" "daemon should be running"
pid1=$(server_pid)
info "daemon pid1=${pid1}"

info "freeze it: alive, holding the lifetime lock, answering nothing"
kill -STOP "${pid1}"

info "next command must replace it"
set +e
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null 2>&1; rc=$?
set -e
[ "${rc}" -eq 0 ] || fail "command failed against an unresponsive daemon (rc=${rc})"
assert_equals "1" "$(server_count)" "exactly one daemon after the replacement"
pid2=$(server_pid)
info "daemon pid2=${pid2}"
[ "${pid2}" != "${pid1}" ] || fail "the unresponsive daemon was NOT replaced (pid unchanged: ${pid1})"
grep -q "fleet server replaces an unresponsive one" "${HOME}/.fleet/fleet.log" \
  || fail "the replacement did not record why the previous daemon vanished"

info "the replacement is healthy: a further command must leave it alone"
"${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" >/dev/null
assert_equals "${pid2}" "$(server_pid)" "a healthy daemon must not be replaced"

pass "unresponsive daemon replaced"
