#!/usr/bin/env bash
# itest: no-docker
# Description: a remote TUI over the fleet gateway connects a cluster from fleetd's host kubeconfig, its Connect page form setting the name and namespace, and sets the Storage pickers (shared and instance-disk classes), Allow privileged and Extra PATH exactly as a local TUI does; [L] Run login here is not offered remotely
set -euo pipefail

source "$(dirname "$0")/../common.sh"

GW_PID=""
scratch=""
itest_cleanup() {
  tui_kill
  if [ -n "${GW_PID}" ]; then
    kill "${GW_PID}" 2>/dev/null || true
    wait "${GW_PID}" 2>/dev/null || true
  fi
  [ -z "${scratch}" ] || rm -rf "${scratch}"
}
itest_begin

# free_port prints a free localhost TCP port.
free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

# A fresh ~/.fleet; no fleet or instance is needed (see 201 for why not
# setup_test's backend step).
rm -rf "${HOME}/.fleet"
mkdir -p "${HOME}/.fleet"
scratch=$(mktemp -d)
kubeconfig="${scratch}/kubeconfig"
itest_fake_kubeconfig "${kubeconfig}"

# fleetd registers with a local gateway that offers remote Fleet (as 300).
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
"${FLEET_BIN}" gateway \
  --public-url "http://127.0.0.1:${PUBLIC_PORT}" \
  --public-grpc-url "${GATEWAY_URL}" \
  --public-addr "127.0.0.1:${PUBLIC_PORT}" \
  --grpc-addr "127.0.0.1:${GRPC_PORT}" \
  >"${scratch}/gateway.log" 2>&1 &
GW_PID=$!
for _ in $(seq 1 "$(_scale_timeout 50)"); do
  curl -fsS "http://127.0.0.1:${PUBLIC_PORT}/healthz" >/dev/null 2>&1 && break
  kill -0 "${GW_PID}" 2>/dev/null || { cat "${scratch}/gateway.log"; fail "gateway process exited early"; }
  sleep 0.2
done

info "start fleetd locally; it registers with the gateway"
"${FLEET_BIN}" ls >/dev/null
session_file="${HOME}/.fleet/gateway_session.json"
for _ in $(seq 1 "$(_scale_timeout 100)"); do
  [ -f "${session_file}" ] && break
  sleep 0.2
done
assert_file_exists "${session_file}"
public_grpc_url=$(grep -oE '"public_grpc_url"[[:space:]]*:[[:space:]]*"[^"]+"' "${session_file}" | sed -E 's/.*"([^"]+)"$/\1/')
[ -n "${public_grpc_url}" ] || fail "no public_grpc_url in ${session_file}"

# The remote TUI: the same binary with FLEET_GATEWAY/FLEET_TOKEN (the tmux
# session would not inherit them from this script).
remote="${scratch}/fleet-remote"
cat > "${remote}" <<EOF
#!/bin/sh
export FLEET_GATEWAY='${public_grpc_url}'
export FLEET_TOKEN='$(cat "${HOME}/.fleet/mcp.token")'
exec '${FLEET_BIN}' "\$@"
EOF
chmod +x "${remote}"
FLEET_BIN="${remote}" tui_spawn
tui_wait_for "No instances" 20
# The Armada selector names the boot remote: this TUI is on the gateway.
tui_wait_for "127.0.0.1 (env)" 10
tui_send Enter
tui_wait_for "Tmux vim keys" 10
tui_wait_for "contexts from fleetd" 10

info "the remote picker reads a kubeconfig on fleetd's host"
tui_cursor_to '\+ Kubernetes Cluster'
tui_send Enter
tui_wait_for "Connect a Kubernetes cluster" 10
tui_send C-o
sleep 0.3
tui_send C-u
tui_send_text "${kubeconfig}"
tui_send Enter
tui_wait_for "itest-plugin" 15
tui_cursor_to 'itest-plugin'
tui_wait_for "exec: itest-login-plugin" 5
tui_assert_not_contains "[L] login here" "[L] runs the plugin on this machine: never offered over the gateway"
tui_cursor_to '\* itest-dead'

info "choose it, set its name and namespace on the form, then connect the failing context anyway"
tui_send Enter
tui_wait_for "connect anyway" 60
tui_cursor_to 'Name:'
tui_send Enter
sleep 0.3
tui_send C-u
tui_send_text "remote-dead"
tui_send Enter
tui_cursor_to 'Namespace:'
tui_send Enter
sleep 0.3
tui_send C-u
tui_send_text "remote-ns"
tui_send Enter
sleep 0.5 # the new namespace is tested again
tui_wait_for "connect anyway" 60
tui_cursor_to '\[ Connect anyway \]'
tui_send Enter
tui_wait_for_absent "Connect a Kubernetes cluster" 20
tui_wait_for "☸ remote-dead" 5
[ "$(itest_kube_clusters 'print scalar @c')" = 1 ] || fail "fleetd should hold one connection"
[ "$(itest_kube_clusters 'print "$c[0]{display_name} $c[0]{namespace}"')" = "remote-dead remote-ns" ] \
  || fail "the form's name and namespace set over the gateway were not saved: $(cat "${HOME}/.fleet/kube_clusters.json")"

info "the remote editor sets the Storage pickers, Allow privileged and Extra PATH"
tui_cursor_to '☸ remote-dead'
tui_send Enter
tui_wait_for "Edit cluster ☸ remote-dead" 5
# The Storage pickers: fleetd lists no class for a dead context, so each
# takes a typed name, as a cluster that forbids listing them does.
tui_cursor_to '▶ Storage'
tui_send l
tui_cursor_to 'Shared storage:'
tui_send Enter
sleep 0.3
tui_send_text "itest-shared"
tui_send Enter
tui_wait_for "Saved Shared storage" 15
tui_cursor_to 'Instance disks:'
tui_send Enter
sleep 0.3
tui_send_text "itest-disks"
tui_send Enter
tui_wait_for "Saved Instance disks" 15
[ "$(itest_kube_clusters 'print "$c[0]{shared_storage_class} $c[0]{storage_class}"')" = "itest-shared itest-disks" ] \
  || fail "the storage classes picked over the gateway were not saved: $(itest_kube_clusters 'print "$c[0]{shared_storage_class} $c[0]{storage_class}"')"
tui_cursor_to '▶ Security'
tui_send l
tui_cursor_to 'Allow privileged:'
tui_send Right
tui_wait_for "Privileged pods allowed" 15
tui_cursor_to '▶ Advanced'
tui_send l
tui_cursor_to 'Extra PATH:'
tui_send Enter
sleep 0.3
tui_send_text "/opt/itest/bin"
tui_send Enter
tui_wait_for "Saved Extra PATH" 15
[ "$(itest_kube_clusters 'print $c[0]{allow_privileged} ? "on" : "off"')" = on ] || fail "allow_privileged set over the gateway was not saved"
[ "$(itest_kube_clusters 'print join(":", @{$c[0]{extra_path}})')" = /opt/itest/bin ] || fail "extra_path set over the gateway was not saved"
tui_send Escape
tui_wait_for_absent "Edit cluster" 5

info "and disconnects it"
tui_cursor_to '☸ remote-dead'
tui_send l
tui_send l
tui_send Enter
tui_wait_for "Disconnect ☸ remote-dead" 5
tui_send Enter
tui_wait_for "Disconnected ☸ remote-dead" 20
[ "$(itest_kube_clusters 'print scalar @c')" = 0 ] || fail "the connection must be gone"

pass "a remote TUI over the gateway connects (the form's name and namespace), edits (Storage pickers, Allow privileged, Extra PATH) and disconnects a cluster as a local one does"
