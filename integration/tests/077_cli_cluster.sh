#!/usr/bin/env bash
# itest: no-docker
# itest: k8s
# Description: `fleet cluster` contexts/add/set/ls/test/gc/login/rm work on the daemon host's kubeconfig and print the same through the gateway as locally; on a cluster backend a plain namespace tests usable and a baseline-labelled one limited.
set -euo pipefail

source "$(dirname "$0")/../common.sh"

GW_PID=""
LIVE_NAMESPACES=""
itest_cleanup() {
  if [ -n "${GW_PID}" ]; then
    kill "${GW_PID}" 2>/dev/null || true
    wait "${GW_PID}" 2>/dev/null || true
  fi
  # Fleet never deletes a namespace: the ones this test made go here.
  for ns in ${LIVE_NAMESPACES}; do
    kubectl --context "${FLEET_ITEST_CLUSTER}" delete namespace "${ns}" --wait=false >/dev/null 2>&1 || true
  done
}
itest_begin

# Host-side: cluster connections only, no instance. setup_test is not used:
# there is no fixture repo to clone, and its kubernetes half would register
# FLEET_ITEST_CLUSTER itself, which is what this test does by hand.
rm -rf "${HOME}/.fleet" "${TEST_SCRATCH_DIR}"
mkdir -p "${HOME}/.fleet" "${TEST_SCRATCH_DIR}"

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

# --- the daemon host's kubeconfig: two contexts on a cluster that is not there
KUBECONFIG_FILE="${TEST_SCRATCH_DIR}/kube/config"
EXTRA_DIR="${TEST_SCRATCH_DIR}/sdk/bin"
mkdir -p "$(dirname "${KUBECONFIG_FILE}")" "${EXTRA_DIR}"
KUBE_TOKEN="itest-SECRET-kube-token"
cat > "${KUBECONFIG_FILE}" <<EOF
apiVersion: v1
kind: Config
current-context: offline
clusters:
- name: nowhere
  cluster: {server: "https://127.0.0.1:1"}
contexts:
- name: offline
  context: {cluster: nowhere, user: tok, namespace: team}
- name: offline-b
  context: {cluster: nowhere, user: tok}
users:
- name: tok
  user: {token: ${KUBE_TOKEN}}
EOF

# --- a gateway, and remote fleet on, before the first command spawns fleetd
PUBLIC_PORT="$(free_port)"
GRPC_PORT="$(free_port)"
while [ "${GRPC_PORT}" = "${PUBLIC_PORT}" ]; do GRPC_PORT="$(free_port)"; done
GATEWAY_URL="http://127.0.0.1:${GRPC_PORT}"
cat > "${HOME}/.fleet/config.json" <<EOF
{
  "remote_mcp_settings": {
    "enabled": true,
    "fleet_enabled": true,
    "gateway_url": "${GATEWAY_URL}"
  }
}
EOF
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

# The first command spawns fleetd, which registers with the gateway.
out=$("${FLEET_BIN}" cluster ls)
assert_contains "${out}" "No cluster connections" "a fresh daemon lists connections"
session_file="${HOME}/.fleet/gateway_session.json"
for _ in $(seq 1 "$(_scale_timeout 100)"); do
  [ -f "${session_file}" ] && grep -q '"public_grpc_url"' "${session_file}" && break
  sleep 0.2
done
public_grpc_url=$(grep -oE '"public_grpc_url"[[:space:]]*:[[:space:]]*"[^"]+"' "${session_file}" 2>/dev/null | sed -E 's/.*"([^"]+)"$/\1/' || true)
[ -n "${public_grpc_url}" ] || { cat "${gw_log}" || true; fail "fleetd never registered with the gateway"; }
info "gateway grpc url: ${public_grpc_url}"

# session prints one `fleet cluster` run: each command, its output and its
# non-zero exit. The same session runs locally and through the gateway.
session() {
  run() {
    printf '$ fleet %s\n' "$*"
    local rc=0
    "${FLEET_BIN}" "$@" 2>&1 || rc=$?
    [ "${rc}" -eq 0 ] || printf 'exit %s\n' "${rc}"
  }
  run cluster contexts --kubeconfig "${KUBECONFIG_FILE}"
  run cluster add offline --kubeconfig "${KUBECONFIG_FILE}" allow_privileged=true "extra_path=${EXTRA_DIR}" \
    plugin_env=AWS_PROFILE "pod_overrides_yaml=priorityClassName: fleet"
  run cluster add offline-b --name second --kubeconfig "${KUBECONFIG_FILE}" -n other
  run cluster set offline max_instances=3 image.mode=registry image.registry=registry.example.com/team \
    node_selector=pool=fleet tolerations=dedicated=fleet:NoExecute:60 resources.cpu_request=2 debug_capabilities=require
  run cluster set second display_name=renamed allow_privileged=false
  # A test's result is the connection's health from then on, so the listings
  # that follow do not depend on the background probe's timing.
  run cluster test offline
  run cluster test renamed
  run cluster ls
  run cluster ls offline
  run cluster test --context offline-b --kubeconfig "${KUBECONFIG_FILE}" -n other block_metadata=false
  run cluster gc offline
  run cluster login offline
  run cluster set offline nope=1
  run cluster rm offline --forget
  run cluster rm renamed
  run cluster ls
}

# ids are random per add and the timestamps differ; an add replies before
# (or, rarely, after) the first background probe.
normalize() {
  sed -E -e 's/(offline(-b)?)-[a-z0-9]{6}([^a-z0-9-]|$)/\1-<id>\3/g' \
    -e 's/^(created_at|updated_at)=.*/\1=<time>/' \
    -e 's/; (checking|unreachable)$/; <state>/'
}

info "local session"
local_out=$(session)
printf '%s\n' "${local_out}"

info "the same session through the gateway"
remote_out=$(
  export FLEET_GATEWAY="${public_grpc_url}"
  FLEET_TOKEN="$(cat "${HOME}/.fleet/mcp.token")"
  export FLEET_TOKEN
  session
)

local_norm="${TEST_SCRATCH_DIR}/local.txt"
remote_norm="${TEST_SCRATCH_DIR}/remote.txt"
printf '%s\n' "${local_out}" | normalize > "${local_norm}"
printf '%s\n' "${remote_out}" | normalize > "${remote_norm}"
if ! diff -u "${local_norm}" "${remote_norm}"; then
  fail "fleet cluster prints differently through the gateway"
fi

assert_contains "${local_out}" "offline    nowhere  team       token" "contexts lists the daemon host's kubeconfig"
assert_contains "${local_out}" "on fleetd @ " "contexts names the daemon host"
assert_contains "${local_out}" "Added cluster \"offline\" (offline-" "add names a connection after its context"
assert_contains "${local_out}" "Added cluster \"second\" (offline-b-" "add --name"
assert_contains "${local_out}" "Updated cluster \"offline\": max_instances, image.mode, image.registry, node_selector, tolerations, resources.cpu_request, debug_capabilities" "set names the fields it changed"
assert_contains "$(cat "${local_norm}")" "renamed  offline-b-<id>" "set display_name renames"
for line in "allow_privileged=true" "extra_path=${EXTRA_DIR}" "plugin_env=AWS_PROFILE" "pod_overrides_yaml=priorityClassName: fleet" \
  "max_instances=3" "image.mode=registry" "image.registry=registry.example.com/team" "node_selector=pool=fleet" \
  "tolerations=dedicated=fleet:NoExecute:60" "resources.cpu_request=2" "debug_capabilities=require" "kubeconfig_paths=${KUBECONFIG_FILE}"; do
  printf '%s\n' "${local_out}" | grep -qxF -- "${line}" || fail "ls offline does not show ${line}"
done
# Unreachable: each test exits 1 after its result line, saved or draft.
test_out=$(printf '%s\n' "${local_out}" | sed -n '/^\$ fleet cluster test offline$/,/^\$ /p')
assert_contains "${test_out}" "reachable               fail" "test offline reports the unreachable API server"
assert_contains "${test_out}" "unreachable: " "test offline ends with the state"
assert_contains "${test_out}" "exit 1" "test of an unreachable cluster exits 1"
draft_out=$(printf '%s\n' "${local_out}" | sed -n '/^\$ fleet cluster test --context/,/^\$ /p')
assert_contains "${draft_out}" "exit 1" "a draft test of an unreachable context exits 1"
assert_contains "${local_out}" "does not log in through a credential plugin (it uses token)" "login explains a token context"
assert_contains "${local_out}" "unknown field \"nope\"" "set refuses an unknown field"
assert_contains "${local_out}" "Removed cluster \"offline\"" "rm --forget"
assert_contains "${local_out}" "Removed cluster \"renamed\"" "rm"
assert_not_contains "${local_out}${remote_out}" "${KUBE_TOKEN}" "no output carries the kubeconfig token"

# --- on a cluster backend: the real context, local and through the gateway
if itest_backend_is kubernetes; then
  real_kubeconfig="${KUBECONFIG:-${HOME}/.kube/config}"
  real_kubeconfig="${real_kubeconfig%%:*}"
  suffix="$(date +%s)"
  for where in local gateway; do
    plain="fleet-it-077-${where}-plain-${suffix}"
    baseline="fleet-it-077-${where}-baseline-${suffix}"
    LIVE_NAMESPACES="${LIVE_NAMESPACES} ${plain} ${baseline}"
    if [ "${where}" = gateway ]; then
      export FLEET_GATEWAY="${public_grpc_url}"
      FLEET_TOKEN="$(cat "${HOME}/.fleet/mcp.token")"
      export FLEET_TOKEN
    fi
    info "${where}: connect ${FLEET_ITEST_CLUSTER} in ${plain} and ${baseline}"
    out=$("${FLEET_BIN}" cluster add "${FLEET_ITEST_CLUSTER}" --name "plain-${where}" --kubeconfig "${real_kubeconfig}" \
      -n "${plain}" --create-namespace)
    assert_contains "${out}" "app.kubernetes.io/managed-by: fleet" "${where}: add --create-namespace prints the namespace"
    out=$("${FLEET_BIN}" cluster add "${FLEET_ITEST_CLUSTER}" --name "baseline-${where}" --kubeconfig "${real_kubeconfig}" \
      -n "${baseline}" --create-namespace --namespace-label pod-security.kubernetes.io/enforce=baseline)
    assert_contains "${out}" "pod-security.kubernetes.io/enforce: baseline" "${where}: the namespace gets its label"

    rc=0
    out=$("${FLEET_BIN}" cluster test "plain-${where}") || rc=$?
    printf '%s\n' "${out}"
    [ "${rc}" -eq 0 ] || fail "${where}: test of a plain namespace exited ${rc}"
    last=$(printf '%s\n' "${out}" | tail -n 1)
    case "${last}" in
      ready*|limited*) ;;
      *) fail "${where}: plain namespace ended '${last}', want ready or limited" ;;
    esac

    rc=0
    out=$("${FLEET_BIN}" cluster test "baseline-${where}") || rc=$?
    printf '%s\n' "${out}"
    [ "${rc}" -eq 0 ] || fail "${where}: test of a baseline namespace exited ${rc}"
    assert_contains "$(printf '%s\n' "${out}" | tail -n 1)" "limited" "${where}: a baseline namespace is limited"
    assert_contains "${out}" "admission privileged    fail" "${where}: baseline refuses a privileged pod"

    out=$("${FLEET_BIN}" cluster gc "plain-${where}")
    assert_contains "${out}" "lease held by this daemon" "${where}: gc runs under this daemon's Lease"
    out=$("${FLEET_BIN}" cluster ls)
    assert_contains "${out}" "baseline-${where}" "${where}: ls lists the connection"
    "${FLEET_BIN}" cluster rm "plain-${where}" >/dev/null
    "${FLEET_BIN}" cluster rm "baseline-${where}" >/dev/null
    unset FLEET_GATEWAY FLEET_TOKEN
  done
fi

pass "fleet cluster works the same locally and through the gateway"
