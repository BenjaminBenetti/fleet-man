#!/usr/bin/env bash
# Description: a devcontainer.json with `--add-host=host.docker.internal:host-gateway` reaches services on the daemon host by that name from inside the instance.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
source "$(dirname "$0")/../net/net.sh"
workdir=$(mktemp -d)
server_pid=""
itest_cleanup() {
  [ -z "${server_pid}" ] || kill "${server_pid}" 2>/dev/null || true
  rm -rf "${workdir}"
}
itest_begin

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-hostgw" setup_test

info "serve a token over HTTP on the daemon host (all interfaces)"
mkdir -p "${workdir}/www"
token="fleet-itest-host-$$-$(date +%s)"
printf '%s\n' "${token}" > "${workdir}/www/token"
port=$(itest_net_free_port)
python3 -m http.server "${port}" --bind 0.0.0.0 --directory "${workdir}/www" > "${workdir}/server.log" 2>&1 &
server_pid=$!
itest_net_wait_accepts "${port}" 15 || fail "the host HTTP server never listened: $(cat "${workdir}/server.log")"

fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

info "host.docker.internal resolves inside the instance"
resolved=$("${FLEET_BIN}" exec "${inst}" -- getent hosts host.docker.internal || true)
[ -n "${resolved}" ] || fail "host.docker.internal does not resolve in the instance"
info "host.docker.internal -> ${resolved%% *}"

info "the instance reaches the host service by that name"
got=$("${FLEET_BIN}" exec "${inst}" -- curl -fsS --max-time 15 "http://host.docker.internal:${port}/token") \
  || fail "curl to host.docker.internal:${port} failed: $(cat "${workdir}/server.log")"
assert_equals "${token}" "${got}" "the token served on the daemon host"

pass "host.docker.internal reaches the daemon host"
