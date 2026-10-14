#!/usr/bin/env bash
# itest: no-docker
# Description: a remote TUI over the fleet gateway sets what a local one does in a fleet's dialogs: a custom mount's daemon-host folder ([f] in the fleet options) and an automation agent's target, a cluster connection (the agent dialog's Backend row); both reach fleetd's state.
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

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}
# fleet_setting <perl on $f, the fleet's settings> — read fleetd's state.
fleet_setting() {
  perl -MJSON::PP -e 'local $/; my $j = decode_json(<STDIN>); my $f = $j->{fleets}{"itest-fleet"}{settings} || {}; '"$1" \
    < "${HOME}/.fleet/state.json"
}

# A fresh ~/.fleet with one fleet, a custom mount and no instance: nothing
# here needs a container or a cluster that answers.
rm -rf "${HOME}/.fleet"
mkdir -p "${HOME}/.fleet"
# A short dir: the fleet options dialog is 50 columns wide, and a longer host
# folder (one under macOS's $TMPDIR) wraps "host: <folder>" across lines.
scratch=$(mktemp -d /tmp/fleet-dialogs.XXXXXX)
host_folder="${scratch}/host-data"
mkdir -p "${host_folder}"
cat > "${HOME}/.fleet/state.json" <<EOF
{
  "fleets": {
    "itest-fleet": {
      "name": "itest-fleet", "remote": "${scratch}/repo",
      "settings": { "customMounts": ["/opt/data"], "homeDir": "/home/vscode" },
      "instances": []
    }
  }
}
EOF
kubeconfig="${scratch}/kubeconfig"
itest_fake_kubeconfig "${kubeconfig}"

# fleetd registers with a local gateway that offers remote Fleet (as 202).
PUBLIC_PORT="$(free_port)"
GRPC_PORT="$(free_port)"
while [ "${GRPC_PORT}" = "${PUBLIC_PORT}" ]; do GRPC_PORT="$(free_port)"; done
GATEWAY_URL="http://127.0.0.1:${GRPC_PORT}"
cat > "${HOME}/.fleet/config.json" <<EOF
{ "remote_mcp_settings": { "enabled": true, "fleet_enabled": true, "gateway_url": "${GATEWAY_URL}" } }
EOF
"${FLEET_BIN}" gateway --public-url "http://127.0.0.1:${PUBLIC_PORT}" --public-grpc-url "${GATEWAY_URL}" \
  --public-addr "127.0.0.1:${PUBLIC_PORT}" --grpc-addr "127.0.0.1:${GRPC_PORT}" >"${scratch}/gateway.log" 2>&1 &
GW_PID=$!
for _ in $(seq 1 "$(_scale_timeout 50)"); do
  curl -fsS "http://127.0.0.1:${PUBLIC_PORT}/healthz" >/dev/null 2>&1 && break
  kill -0 "${GW_PID}" 2>/dev/null || { cat "${scratch}/gateway.log"; fail "gateway process exited early"; }
  sleep 0.2
done

info "fleetd starts, registers with the gateway, and holds a cluster connection"
"${FLEET_BIN}" cluster add itest-dead --kubeconfig "${kubeconfig}" --name itest-dead >/dev/null 2>&1 \
  || fail "fleet cluster add itest-dead failed"
conn=$(itest_kube_clusters 'print $c[0]{id}')
[ -n "${conn}" ] || fail "no connection was added"
session_file="${HOME}/.fleet/gateway_session.json"
for _ in $(seq 1 "$(_scale_timeout 100)"); do
  [ -f "${session_file}" ] && break
  sleep 0.2
done
public_grpc_url=$(grep -oE '"public_grpc_url"[[:space:]]*:[[:space:]]*"[^"]+"' "${session_file}" | sed -E 's/.*"([^"]+)"$/\1/')
[ -n "${public_grpc_url}" ] || fail "no public_grpc_url in ${session_file}"
remote="${scratch}/fleet-remote"
cat > "${remote}" <<EOF
#!/bin/sh
export FLEET_GATEWAY='${public_grpc_url}'
export FLEET_TOKEN='$(cat "${HOME}/.fleet/mcp.token")'
exec '${FLEET_BIN}' "\$@"
EOF
chmod +x "${remote}"
FLEET_BIN="${remote}" tui_spawn
tui_wait_for "itest-fleet" 20
tui_wait_for "127.0.0.1 (env)" 10

info "the fleet options: [f] gives /opt/data a folder on fleetd's host"
tui_send e
tui_wait_for "Edit fleet" 5
tui_cursor_to '▶ Custom mounts \(1\)'
tui_send l
tui_cursor_to '/opt/data'
tui_send f
tui_wait_for "a folder on the daemon host" 5
tui_send_text "${host_folder}"
tui_send Enter
tui_wait_for "host: ${host_folder}" 15
deadline=$(( $(date +%s) + $(_scale_timeout 15) ))
until [ "$(fleet_setting 'print $f->{customMountHostFolders}{"/opt/data"} // ""')" = "${host_folder}" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the host folder set over the gateway was not saved: $(fleet_setting 'print JSON::PP->new->encode($f->{customMountHostFolders} // {})')"
  sleep 0.3
done
tui_send Escape
tui_wait_for_absent "Edit fleet" 5

info "the agent dialog: an agent on the cluster connection"
tui_send m
tui_wait_for "+ add agent" 5
tui_cursor_to '\+ add agent'
tui_send Enter
tui_wait_for "New agent" 5
tui_send Enter
sleep 0.2
tui_send_text "remote-agent"
sleep 0.2
tui_send Enter
tui_cursor_to 'Backend:'
for _ in $(seq 1 6); do
  tui_capture | grep -E '>[[:space:]]+Backend:' | grep -q '☸ itest-dead' && break
  tui_send Right
  sleep 0.3
done
tui_capture | grep -E '>[[:space:]]+Backend:' | grep -q '☸ itest-dead' \
  || { tui_capture >&2; fail "the agent dialog's Backend row never offered the cluster connection"; }
tui_cursor_to '\[ Save \]'
tui_send Enter
tui_wait_for "agents (1)" 15
assert_equals "kubernetes ${conn}" \
  "$(fleet_setting 'my ($a) = grep { $_->{name} eq "remote-agent" } @{$f->{agents} || []}; print "$a->{backend} $a->{kube_cluster}"')" \
  "the agent saved over the gateway runs on the connection"

pass "over the gateway: a custom mount's host folder and an agent's cluster target set from the TUI's dialogs"
