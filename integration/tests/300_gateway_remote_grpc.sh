#!/usr/bin/env bash
# Description: remote gRPC control through the fleet gateway — a remote `fleet` client drives a registered fleetd over the gateway and the commanded action takes effect.
set -euo pipefail

source "$(dirname "$0")/../common.sh"

GW_PID=""
itest_cleanup() {
  if [ -n "${GW_PID}" ]; then
    kill "${GW_PID}" 2>/dev/null || true
    wait "${GW_PID}" 2>/dev/null || true
  fi
}
itest_begin

# free_port prints a free localhost TCP port (python3 binds :0; /dev/tcp fallback).
free_port() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
    return
  fi
  local p
  for _ in $(seq 1 100); do
    p=$(( (RANDOM % 20000) + 30000 ))
    if ! (exec 3<>"/dev/tcp/127.0.0.1/${p}") 2>/dev/null; then
      printf '%s' "${p}"
      return
    fi
    exec 3>&- 2>/dev/null || true
  done
  fail "could not find a free port"
}

setup_test

PUBLIC_PORT="$(free_port)"
GRPC_PORT="$(free_port)"
while [ "${GRPC_PORT}" = "${PUBLIC_PORT}" ]; do GRPC_PORT="$(free_port)"; done
GATEWAY_URL="http://127.0.0.1:${GRPC_PORT}"
info "gateway public=127.0.0.1:${PUBLIC_PORT} grpc=127.0.0.1:${GRPC_PORT}"

# 1. Enable remote MCP AND remote fleet in config, pointing fleetd at the
#    gateway's gRPC endpoint. fleetd reconciles this at startup and registers
#    over the gRPC Register stream; without fleet_enabled it would not negotiate
#    the grpc tunnel feature and the gateway would refuse remote control RPCs.
cat > "${HOME}/.fleet/config.json" <<EOF
{
  "remote_mcp_settings": {
    "enabled": true,
    "fleet_enabled": true,
    "gateway_url": "${GATEWAY_URL}"
  }
}
EOF

# 2. Start the gateway in the background, cert-less (plain HTTP public + h2c gRPC).
#    --public-grpc-url makes the gateway hand grpc-negotiating daemons their
#    Public GRPC URL — the exact FLEET_GATEWAY value used below.
gw_log="${TEST_SCRATCH_DIR}/gateway.log"
"${FLEET_BIN}" gateway \
  --public-url "http://127.0.0.1:${PUBLIC_PORT}" \
  --public-grpc-url "${GATEWAY_URL}" \
  --public-addr "127.0.0.1:${PUBLIC_PORT}" \
  --grpc-addr "127.0.0.1:${GRPC_PORT}" \
  >"${gw_log}" 2>&1 &
GW_PID=$!

# Wait for the gateway's public listener (GET /healthz -> "ok").
gw_up=false
for _ in $(seq 1 "$(_scale_timeout 50)"); do
  if curl -fsS "http://127.0.0.1:${PUBLIC_PORT}/healthz" >/dev/null 2>&1; then
    gw_up=true
    break
  fi
  kill -0 "${GW_PID}" 2>/dev/null || { cat "${gw_log}" || true; fail "gateway process exited early"; }
  sleep 0.2
done
[ "${gw_up}" = true ] || { cat "${gw_log}" || true; fail "gateway /healthz never came up"; }
info "gateway is up"

# 3. Create a running instance LOCALLY (unix socket). This spawns fleetd, which
#    reads the config above and registers with the gateway.
info "fleet up alpha (local)"
fleet_up alpha

# 4. Wait for fleetd to register — it persists the assigned session to
#    ~/.fleet/gateway_session.json on success.
session_file="${HOME}/.fleet/gateway_session.json"
for _ in $(seq 1 "$(_scale_timeout 100)"); do
  [ -f "${session_file}" ] && break
  sleep 0.2
done
assert_file_exists "${session_file}"
info "registered: $(cat "${session_file}")"

# 5. FLEET_GATEWAY is the gateway-computed Public GRPC URL
#    (<public-grpc-url>/grpc/<id>), persisted in the session file — exactly what
#    the TUI shows the user to feed a remote fleet.
public_grpc_url=$(grep -oE '"public_grpc_url"[[:space:]]*:[[:space:]]*"[^"]+"' "${session_file}" | sed -E 's/.*"([^"]+)"$/\1/')
[ -n "${public_grpc_url}" ] || fail "no public_grpc_url in session file — gateway did not hand one out"
export FLEET_GATEWAY="${public_grpc_url}"
export FLEET_TOKEN="$(cat "${HOME}/.fleet/mcp.token")"
info "FLEET_GATEWAY=${FLEET_GATEWAY}"

# 6. REMOTE read through the gateway: ls reports the running instance.
remote_ls=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
printf '%s\n' "${remote_ls}"
assert_contains "${remote_ls}" "alpha"   "remote ls (via gateway) missing the instance"
assert_contains "${remote_ls}" "running" "remote ls should show running before the stop"

# 7. REMOTE mutation through the gateway: stop the instance.
info "remote: fleet stop alpha (via gateway)"
remote_stop=$("${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/alpha")
printf '%s\n' "${remote_stop}"
assert_contains "${remote_stop}" "stopped" "remote stop output should mention stopped"

# 8. Verify LOCALLY (unix socket, no gateway) that fleetd actually took the action.
unset FLEET_GATEWAY FLEET_TOKEN
local_ls=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
printf '%s\n' "${local_ls}"
assert_contains "${local_ls}" "stopped" "instance should be stopped locally after the remote command"

# And the container itself is no longer running.
container_id=$(grep -oE '"container_id"[[:space:]]*:[[:space:]]*"[^"]+"' "${HOME}/.fleet/state.json" | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
if [ -n "${container_id}" ] && command -v docker >/dev/null 2>&1; then
  docker_state=$(docker inspect -f '{{.State.Status}}' "${container_id}" 2>/dev/null || echo unknown)
  info "docker state after remote stop: ${docker_state}"
  [ "${docker_state}" != "running" ] || fail "container still running after remote stop"
fi

pass "remote gRPC control through the gateway"
