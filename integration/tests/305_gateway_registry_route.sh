#!/usr/bin/env bash
# itest: no-docker
# Description: `fleet gateway` offers the /v2/ image registry route only with a session key and an https --public-url at the host root, and then answers the API root with a Basic challenge and an unknown registry id with 404.
set -euo pipefail

source "$(dirname "$0")/../common.sh"

GW_PID=""
WORK_DIR="$(mktemp -d)"
itest_cleanup() {
  stop_gateway
  rm -rf "${WORK_DIR}"
}
itest_begin

# Host-side only: the gateway alone, no daemon and no instance (a daemon that
# registers for the registry needs the Enable Image Registry setting).

# free_port prints a free localhost TCP port.
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

# start_gateway <log> <public-url> [flags...]: run a cert-less gateway (as
# behind a TLS-terminating proxy) on a fresh port and wait for /healthz.
PUBLIC_PORT=""
start_gateway() {
  local log="$1" url="$2"
  shift 2
  PUBLIC_PORT="$(free_port)"
  "${FLEET_BIN}" gateway --public-url "${url}" --public-addr "127.0.0.1:${PUBLIC_PORT}" \
    --grpc-addr "127.0.0.1:$(free_port)" "$@" >"${log}" 2>&1 &
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

# status_of <path>: the HTTP status of GET <path> on the running gateway.
status_of() {
  curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PUBLIC_PORT}$1"
}

RID="$(printf '%064d' 7)"

# 1. No session key: the route is off, with the reason logged.
log="${WORK_DIR}/no-key.log"
start_gateway "${log}" "https://gw.itest.example"
assert_contains "$(cat "${log}")" "image registry route off" "a gateway without --session-key must log the route off"
assert_contains "$(cat "${log}")" "no --session-key" "the log must name the missing session key"
assert_equals "404" "$(status_of /v2/)" "/v2/ without the registry route"
stop_gateway

# 2. A path prefix or plain http: still off, each with its reason.
log="${WORK_DIR}/prefix.log"
start_gateway "${log}" "https://gw.itest.example/fleet" --session-key itest-key
assert_contains "$(cat "${log}")" "path prefix" "a path-prefixed --public-url must keep the route off"
assert_equals "404" "$(status_of /v2/)" "/v2/ with a path-prefixed public URL"
stop_gateway

log="${WORK_DIR}/http.log"
start_gateway "${log}" "http://gw.itest.example" --session-key itest-key
assert_contains "$(cat "${log}")" "is not https" "an http --public-url must keep the route off"
stop_gateway

# 3. Session key from the environment and an https URL at the host root: on.
log="${WORK_DIR}/on.log"
export FLEET_GATEWAY_SESSION_KEY=itest-key
start_gateway "${log}" "https://gw.itest.example:8443"
unset FLEET_GATEWAY_SESSION_KEY
assert_contains "$(cat "${log}")" "image registry route on" "the route must be on"
assert_contains "$(cat "${log}")" "registry_host=gw.itest.example:8443" "the route's host must be the public URL's host:port"

# Header names compared lowercased: HTTP names are case-insensitive.
headers="$(curl -s -D - -o "${WORK_DIR}/base.json" "http://127.0.0.1:${PUBLIC_PORT}/v2/" | tr 'A-Z' 'a-z')"
assert_contains "${headers}" "401 unauthorized" "/v2/ must answer 401"
assert_contains "${headers}" 'www-authenticate: basic realm="fleet"' "/v2/ must carry a Basic challenge"
assert_contains "${headers}" "docker-distribution-api-version: registry/2.0" "/v2/ must name the distribution API"
assert_contains "$(cat "${WORK_DIR}/base.json")" "UNAUTHORIZED" "/v2/ body must be an OCI error"

body="$(curl -s "http://127.0.0.1:${PUBLIC_PORT}/v2/${RID}/devcontainer/manifests/sha256:0")"
assert_equals "404" "$(status_of "/v2/${RID}/devcontainer/manifests/sha256:0")" "an unknown registry id"
assert_contains "${body}" "NAME_UNKNOWN" "an unknown registry id's body must be an OCI error"
stop_gateway

pass "gateway registry route gated on --session-key and an https root URL"
