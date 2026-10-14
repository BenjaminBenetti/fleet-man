#!/usr/bin/env bash
# itest: k8s
# Description: a devcontainer.json with `--add-host=host.docker.internal:host-gateway` reaches services on the daemon host by that name from inside the instance. On a cluster the name is 127.0.0.2, where the pod agent mirrors the daemon host's listening ports (loopback-only ones too), each connection dialled on the daemon host.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
source "$(dirname "$0")/../net/net.sh"
workdir=$(mktemp -d)
server_pid=""
loopback_pid=""
itest_cleanup() {
  [ -z "${server_pid}" ] || kill "${server_pid}" 2>/dev/null || true
  [ -z "${loopback_pid}" ] || kill "${loopback_pid}" 2>/dev/null || true
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

if itest_backend_is kubernetes; then
  info "a service the daemon host serves on 127.0.0.1 only is mirrored too, once it listens"
  # docker on Linux reaches only services on every interface (the bridge
  # address); the pod's mirror dials the daemon host's loopback.
  lport=$(itest_net_free_port)
  python3 -m http.server "${lport}" --bind 127.0.0.1 --directory "${workdir}/www" > "${workdir}/loopback.log" 2>&1 &
  loopback_pid=$!
  itest_net_wait_accepts "${lport}" 15 || fail "the loopback HTTP server never listened: $(cat "${workdir}/loopback.log")"
  got=""
  deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    got=$("${FLEET_BIN}" exec "${inst}" -- curl -fsS --max-time 5 "http://host.docker.internal:${lport}/token" 2>/dev/null || true)
    [ "${got}" = "${token}" ] && break
    sleep 1
  done
  assert_equals "${token}" "${got}" "the loopback-only service, through the mirror"
  kill "${loopback_pid}" 2>/dev/null || true
  wait "${loopback_pid}" 2>/dev/null || true
  loopback_pid=""
  info "a port the daemon host stops listening at is refused in the pod"
  if "${FLEET_BIN}" exec "${inst}" -- curl -fsS --max-time 5 "http://host.docker.internal:${lport}/token" >/dev/null 2>&1; then
    fail "a port nothing listens at on the daemon host still answered in the pod"
  fi
fi

pass "host.docker.internal reaches the daemon host"
