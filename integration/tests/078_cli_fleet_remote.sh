#!/usr/bin/env bash
# itest: k8s
# Description: the fleet-level settings and actions of the cluster work — a custom mount's daemon-host folder (fleet mount), an agent on a deploy target (fleet agent, --cluster on a cluster backend), the shared-folder sync (fleet sync) — print and save the same through the gateway as locally, and a rebuild through the gateway replaces the instance's container (its next generation on a cluster); on a cluster backend a host write reaches the pod after a `fleet sync wait` through the gateway.
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

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

FLEET="${FIXTURE_REPO_NAME}"
setup_test
# customMounts=["/opt/sync"]: the instance's folder the host write below reaches.
seed_fleet_settings "${FLEET}" false false /home/vscode false false '["/opt/sync"]'

# --- a gateway, and remote fleet on, for the next daemon
PUBLIC_PORT="$(free_port)"
GRPC_PORT="$(free_port)"
while [ "${GRPC_PORT}" = "${PUBLIC_PORT}" ]; do GRPC_PORT="$(free_port)"; done
GATEWAY_URL="http://127.0.0.1:${GRPC_PORT}"
cfg="${HOME}/.fleet/config.json"
[ -s "${cfg}" ] || printf '{}' > "${cfg}"
jq --arg gw "${GATEWAY_URL}" '.remote_mcp_settings = {enabled: true, fleet_enabled: true, gateway_url: $gw}' "${cfg}" > "${cfg}.tmp"
mv "${cfg}.tmp" "${cfg}"
itest_config_written
gw_log="${TEST_SCRATCH_DIR}/gateway.log"
"${FLEET_BIN}" gateway \
  --public-url "http://127.0.0.1:${PUBLIC_PORT}" \
  --public-grpc-url "${GATEWAY_URL}" \
  --public-addr "127.0.0.1:${PUBLIC_PORT}" \
  --grpc-addr "127.0.0.1:${GRPC_PORT}" \
  >"${gw_log}" 2>&1 &
GW_PID=$!
gw_up=false
for _ in $(seq 1 "$(_scale_timeout 50)"); do
  if curl -fsS "http://127.0.0.1:${PUBLIC_PORT}/healthz" >/dev/null 2>&1; then
    gw_up=true
    break
  fi
  kill -0 "${GW_PID}" 2>/dev/null || { cat "${gw_log}" || true; fail "gateway exited early"; }
  sleep 0.2
done
[ "${gw_up}" = true ] || { cat "${gw_log}" || true; fail "gateway /healthz never came up"; }

info "fleet up alpha (custom mount /opt/sync); the daemon registers with the gateway"
fleet_up alpha
inst="${FLEET}/alpha"
session_file="${HOME}/.fleet/gateway_session.json"
for _ in $(seq 1 "$(_scale_timeout 100)"); do
  [ -f "${session_file}" ] && grep -q '"public_grpc_url"' "${session_file}" && break
  sleep 0.2
done
public_grpc_url=$(grep -oE '"public_grpc_url"[[:space:]]*:[[:space:]]*"[^"]+"' "${session_file}" 2>/dev/null | sed -E 's/.*"([^"]+)"$/\1/' || true)
[ -n "${public_grpc_url}" ] || { cat "${gw_log}" || true; fail "fleetd never registered with the gateway"; }
info "gateway grpc url: ${public_grpc_url}"

# gw <args...> — a fleet command through the gateway.
gw() {
  FLEET_GATEWAY="${public_grpc_url}" FLEET_TOKEN="$(cat "${HOME}/.fleet/mcp.token")" "${FLEET_BIN}" "$@"
}

data="${TEST_SCRATCH_DIR}/data"
missing="${TEST_SCRATCH_DIR}/missing"
mkdir -p "${data}"
target=(--backend devcontainer)
if itest_backend_is kubernetes; then
  target=(--cluster itest)
fi

# session <runner> prints one run of the fleet-level commands (through
# <runner>: the binary, or gw), each command, its output and a non-zero
# exit, and leaves the fleet as it found it.
session() {
  local runner=$1
  run() {
    printf '$ fleet %s\n' "$*"
    local rc=0
    if [ "${runner}" = gw ]; then gw "$@" 2>&1 || rc=$?; else "${FLEET_BIN}" "$@" 2>&1 || rc=$?; fi
    [ "${rc}" -eq 0 ] || printf 'exit %s\n' "${rc}"
  }
  run mount add "${FLEET}" /opt/data --host "${data}"
  run mount add "${FLEET}" /opt/gone --host "${missing}"
  run mount add "${FLEET}" /opt/fleet-state --host "${HOME}/.fleet"
  run mount list "${FLEET}"
  run mount add "${FLEET}" /opt/gone
  run mount list "${FLEET}"
  run agent create "${FLEET}" runner "${target[@]}" --command "ANTHROPIC_API_KEY=x claude '\${PROMPT}'"
  run agent create "${FLEET}" needlogin "${target[@]}" --command "claude '\${PROMPT}'"
  run agent list "${FLEET}"
  run sync conflicts "${FLEET}"
  # What it saved, before it goes again.
  jq -c --arg f "${FLEET}" '.fleets[$f].settings | {customMounts, customMountHostFolders, agents: [.agents[]? | {name, backend, kube_cluster: (.kubeCluster // .kube_cluster)}]}' \
    "${HOME}/.fleet/state.json"
  "${FLEET_BIN}" agent delete "${FLEET}" runner >/dev/null 2>&1 || true
  "${FLEET_BIN}" agent delete "${FLEET}" needlogin >/dev/null 2>&1 || true
  "${FLEET_BIN}" mount remove "${FLEET}" /opt/data >/dev/null 2>&1 || true
  "${FLEET_BIN}" mount remove "${FLEET}" /opt/gone >/dev/null 2>&1 || true
}

info "local session"
local_out=$(session local)
printf '%s\n' "${local_out}"
info "the same session through the gateway"
remote_out=$(session gw)
if ! diff -u <(printf '%s\n' "${local_out}") <(printf '%s\n' "${remote_out}"); then
  fail "the fleet-level commands print or save differently through the gateway"
fi
assert_contains "${local_out}" "/opt/data  ${data}" "mount add --host binds the daemon-host folder"
assert_contains "${local_out}" "host folder ${missing} does not exist on the daemon host" "a missing host folder warns"
assert_contains "${local_out}" "overlaps fleet's own state" "~/.fleet is refused as a host folder"
assert_contains "${local_out}" "\"customMountHostFolders\":{\"/opt/data\":\"${data}\"}" "the daemon saved the host folder"
if itest_backend_is kubernetes; then
  assert_contains "${local_out}" "warning: agent needlogin: log in to Claude once for this fleet on ☸ itest" "a cluster agent without a login warns"
  assert_contains "${local_out}" "kubernetes:$(_be_cluster_id)" "agent list names the agent's connection"
fi

info "a host write reaches the instance after fleet sync wait through the gateway"
host_dir=$(itest_share_dir "${FLEET}" .mnt/opt/sync)
mkdir -p "${host_dir}"
printf 'through-the-gateway\n' > "${host_dir}/remote.txt"
out=$(gw sync wait "${FLEET}" --timeout 120s 2>&1) || fail "fleet sync wait through the gateway: ${out}"
assert_equals "through-the-gateway" "$("${FLEET_BIN}" exec "${inst}" -- cat /opt/sync/remote.txt)" "the host's write in the instance"
out=$(gw sync status "${FLEET}" 2>&1) || fail "fleet sync status through the gateway: ${out}"
printf '%s\n' "${out}"
if itest_backend_is kubernetes; then
  assert_contains "${out}" "${FLEET}" "sync status through the gateway names the fleet"
  assert_not_contains "${out}" "nothing to sync" "a fleet with a cluster instance has a sync"
fi

info "fleet rebuild through the gateway"
before=$(itest_container_id alpha)
out=$(gw rebuild "${inst}" 2>&1) || fail "fleet rebuild through the gateway: ${out}"
after=$(itest_container_id alpha)
[ -n "${after}" ] && [ "${after}" != "${before}" ] || fail "the rebuild through the gateway kept container ${before} (now ${after})"
if itest_backend_is kubernetes; then
  case "${after}" in *@g2) ;; *) fail "the rebuilt cluster instance is ${after}, want generation 2" ;; esac
fi
assert_container_running "${after}"
assert_equals "through-the-gateway" "$("${FLEET_BIN}" exec "${inst}" -- cat /opt/sync/remote.txt)" "the custom mount after the rebuild"

pass "fleet mount, agent, sync and rebuild work the same through the gateway as locally"
