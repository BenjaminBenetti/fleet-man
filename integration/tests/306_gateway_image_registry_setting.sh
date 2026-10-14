#!/usr/bin/env bash
# itest: no-docker
# Description: Enable Image Registry alone brings the gateway tunnel up and fleetd's image server answers /v2/<rid>/ through it; a gateway without --session-key serves nothing and fleetd logs why.
set -euo pipefail

source "$(dirname "$0")/../common.sh"

GW_PID=""
itest_cleanup() {
  stop_gateway
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
}
itest_begin

# Host-side only: a cert-less gateway (as behind a TLS-terminating proxy, so
# its --public-url may be https) and the daemon, no cluster and no instance.

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

stop_gateway() {
  if [ -n "${GW_PID}" ]; then
    kill "${GW_PID}" 2>/dev/null || true
    wait "${GW_PID}" 2>/dev/null || true
    GW_PID=""
  fi
}

PUBLIC_PORT="$(free_port)"
GRPC_PORT="$(free_port)"
while [ "${GRPC_PORT}" = "${PUBLIC_PORT}" ]; do GRPC_PORT="$(free_port)"; done

# start_gateway <log> [flags...]: the gateway on the test's fixed ports, up
# once /healthz answers.
start_gateway() {
  local log="$1"
  shift
  "${FLEET_BIN}" gateway --public-url "https://gw.itest.example" --public-addr "127.0.0.1:${PUBLIC_PORT}" \
    --grpc-addr "127.0.0.1:${GRPC_PORT}" "$@" >"${log}" 2>&1 &
  GW_PID=$!
  for _ in $(seq 1 "$(_scale_timeout 50)"); do
    if curl -fsS "http://127.0.0.1:${PUBLIC_PORT}/healthz" >/dev/null 2>&1; then
      return 0
    fi
    kill -0 "${GW_PID}" 2>/dev/null || { cat "${log}" || true; fail "gateway exited early"; }
    sleep 0.2
  done
  cat "${log}" || true
  fail "gateway /healthz never came up"
}

# wait_log <pattern> <what>: wait for fleetd's event log to show pattern.
wait_log() {
  for _ in $(seq 1 "$(_scale_timeout 150)"); do
    if grep -qE -- "$1" "${HOME}/.fleet/fleet.log" 2>/dev/null; then
      return 0
    fi
    sleep 0.2
  done
  cat "${HOME}/.fleet/fleet.log" 2>/dev/null | tail -n 40 || true
  fail "$2"
}

setup_test
pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true

# 1. Only Enable Image Registry is on: no MCP, no remote fleet, no webhook.
cat > "${HOME}/.fleet/config.json" <<EOF
{
  "remote_mcp_settings": {
    "gateway_url": "http://127.0.0.1:${GRPC_PORT}",
    "image_registry_enabled": true
  }
}
EOF

gw_log="${TEST_SCRATCH_DIR}/gateway.log"
mkdir -p "${TEST_SCRATCH_DIR}"
start_gateway "${gw_log}" --session-key itest-registry-key
assert_contains "$(cat "${gw_log}")" "image registry route on" "the gateway must offer the route"

# Any command spawns the daemon, which reads the config and registers.
"${FLEET_BIN}" ls >/dev/null 2>&1 || true
session_file="${HOME}/.fleet/gateway_session.json"
for _ in $(seq 1 "$(_scale_timeout 150)"); do
  grep -q '"public_registry"' "${session_file}" 2>/dev/null && break
  sleep 0.2
done
grep -q '"public_registry"' "${session_file}" 2>/dev/null || { cat "${HOME}/.fleet/fleet.log" 2>/dev/null | tail -n 40; fail "the daemon never registered for the image registry"; }
prefix=$(grep -oE '"public_registry"[[:space:]]*:[[:space:]]*"[^"]+"' "${session_file}" | sed -E 's/.*"([^"]+)"$/\1/')
info "registry prefix ${prefix}"
case "${prefix}" in
  gw.itest.example/*) ;;
  *) fail "the registry prefix ${prefix} does not name the public host" ;;
esac
rid="${prefix#gw.itest.example/}"
assert_equals "64" "${#rid}" "the registry id must be 64 hex digits"
wait_log "remote gateway connected.*publicRegistry=gw.itest.example/${rid}" "fleet.log must record the registry prefix"

# 2. fleetd's image server answers through the tunnel: its own Basic
#    challenge for a request without a credential or with an unknown one
#    (the gateway alone would 404 an id it does not route).
url="http://127.0.0.1:${PUBLIC_PORT}/v2/${rid}/devcontainer/manifests/sha256:$(printf '%064d' 0)"
for _ in $(seq 1 "$(_scale_timeout 50)"); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "${url}")
  [ "${code}" = "401" ] && break
  sleep 0.2
done
headers="$(curl -s -D - -o /dev/null "${url}" | tr 'A-Z' 'a-z')"
assert_contains "${headers}" "401 unauthorized" "fleetd's image server must challenge a request without a credential"
assert_contains "${headers}" 'www-authenticate: basic realm="fleet"' "the challenge must be Basic"
assert_contains "${headers}" "docker-distribution-api-version: registry/2.0" "the answer must come from a registry"
assert_equals "401" "$(curl -s -o /dev/null -w '%{http_code}' -u nobody:nothing "${url}")" "an unknown connection's credential must be refused"
assert_equals "405" "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "${url}")" "the registry must stay pull-only"

# 3. The gateway comes back without --session-key: it no longer offers the
#    route, says why, and fleetd logs the reason it serves no images.
stop_gateway
start_gateway "${TEST_SCRATCH_DIR}/gateway-nokey.log"
assert_contains "$(cat "${TEST_SCRATCH_DIR}/gateway-nokey.log")" "no --session-key" "the gateway must log why the route is off"
wait_log 'remote gateway serves no images for this daemon.*no --session-key' "fleetd must log the gateway's reason"
assert_equals "404" "$(curl -s -o /dev/null -w '%{http_code}' "${url}")" "no route without the registry"

pass "Enable Image Registry alone serves fleetd's image registry through the gateway, and says why when it cannot"
