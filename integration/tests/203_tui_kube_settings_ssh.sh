#!/usr/bin/env bash
# itest: no-docker
# itest: k8s
# Description: a remote TUI over Armada SSH connects a cluster from the remote fleetd's kubeconfig and sets the Storage pickers (shared and instance-disk classes), Allow privileged and Extra PATH exactly as a local TUI does; [L] Run login here is not offered; on a reachable cluster the remote connects it from its own ~/.kube/config
set -euo pipefail

source "$(dirname "$0")/../common.sh"

# The remote is a second user on this host, whose fleetd runs in Remote Fleet
# via SSH mode behind an sshd of this test's own on a free loopback port. The
# invoking user's ~/.ssh is never touched: ssh resolves ~/.ssh from the passwd
# entry, not $HOME, so the local fleetd reaches the remote through an `ssh` on
# its PATH that adds `-F` with this test's key and known_hosts.
ruser=fleet-itest-ssh
scratch=""
sshd_pidfile=""
kube_ctx=""
kube_ns=""
itest_cleanup() {
  tui_kill
  # The local fleetd holds the tunnel and has the ssh wrapper on its PATH.
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  if id "${ruser}" >/dev/null 2>&1; then
    sudo pkill -u "${ruser}" >/dev/null 2>&1 || true
    for _ in $(seq 1 50); do
      pgrep -u "${ruser}" >/dev/null 2>&1 || break
      sleep 0.2
    done
    sudo pkill -9 -u "${ruser}" >/dev/null 2>&1 || true
    sudo userdel -r "${ruser}" >/dev/null 2>&1 || true
  fi
  if [ -n "${sshd_pidfile}" ] && [ -s "${sshd_pidfile}" ]; then
    sudo kill "$(cat "${sshd_pidfile}")" >/dev/null 2>&1 || true
  fi
  if [ -n "${kube_ns}" ]; then
    kubectl --context "${kube_ctx}" delete namespace "${kube_ns}" --wait=false >/dev/null 2>&1 || true
  fi
  [ -z "${scratch}" ] || rm -rf "${scratch}"
}
itest_begin

# free_port prints a free localhost TCP port.
free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

[ "$(uname -s)" = Linux ] || pass "Armada SSH TUI: not run (the second user and its sshd need Linux)"
sudo -n true 2>/dev/null || pass "Armada SSH TUI: not run (needs passwordless sudo for the second user and its sshd)"
sshd_bin=""
for c in /usr/sbin/sshd "$(command -v sshd 2>/dev/null || true)"; do
  if [ -n "${c}" ] && [ -x "${c}" ]; then sshd_bin="${c}"; break; fi
done
if [ -z "${sshd_bin}" ]; then
  command -v apt-get >/dev/null 2>&1 || pass "Armada SSH TUI: not run (no sshd, and no apt-get to install openssh-server)"
  info "installing openssh-server (the QA image ships it)"
  sudo apt-get update -qq >/dev/null
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends openssh-server >/dev/null \
    || fail "could not install openssh-server"
  sshd_bin=/usr/sbin/sshd
fi
ssh_bin=$(command -v ssh) || fail "no ssh client on PATH"

scratch=$(mktemp -d)
chmod 0755 "${scratch}"

# --- the remote user, its sshd and the local side's ssh ---
id "${ruser}" >/dev/null 2>&1 || sudo useradd -m -s /bin/sh "${ruser}"
# '*' rather than useradd's locked '!': sshd lets a key log in to an account
# with no usable password, but not to a locked one.
sudo usermod -p '*' "${ruser}"
rhome=$(getent passwd "${ruser}" | cut -d: -f6)
[ -n "${rhome}" ] || fail "no home directory for ${ruser}"

ssh-keygen -q -t ed25519 -N '' -C fleet-itest -f "${scratch}/id_ed25519"
ssh-keygen -q -t ed25519 -N '' -C fleet-itest-host -f "${scratch}/host_ed25519"
sudo -u "${ruser}" -H sh -c 'umask 077 && mkdir -p ~/.ssh && cat > ~/.ssh/authorized_keys' < "${scratch}/id_ed25519.pub"

port="$(free_port)"
sshd_pidfile="${scratch}/sshd.pid"
sudo mkdir -p /run/sshd
# -f /dev/null: none of the host's sshd_config, only these settings.
sudo "${sshd_bin}" -f /dev/null -h "${scratch}/host_ed25519" -p "${port}" \
  -o ListenAddress=127.0.0.1 -o PidFile="${sshd_pidfile}" -o AllowUsers="${ruser}" \
  -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o AllowTcpForwarding=yes \
  || fail "could not start sshd on 127.0.0.1:${port}"

printf '[127.0.0.1]:%s %s\n' "${port}" "$(cut -d' ' -f1,2 "${scratch}/host_ed25519.pub")" > "${scratch}/known_hosts"
cat > "${scratch}/ssh_config" <<EOF
Host *
  IdentityFile ${scratch}/id_ed25519
  IdentitiesOnly yes
  IdentityAgent none
  UserKnownHostsFile ${scratch}/known_hosts
  GlobalKnownHostsFile /dev/null
EOF
mkdir -p "${scratch}/bin"
cat > "${scratch}/bin/ssh" <<EOF
#!/bin/sh
exec '${ssh_bin}' -F '${scratch}/ssh_config' "\$@"
EOF
chmod +x "${scratch}/bin/ssh"
ok=""
for _ in $(seq 1 "$(_scale_timeout 50)"); do
  if "${scratch}/bin/ssh" -o BatchMode=yes -p "${port}" "${ruser}@127.0.0.1" true 2>"${scratch}/ssh.err"; then ok=1; break; fi
  sleep 0.2
done
[ -n "${ok}" ] || fail "ssh to ${ruser}@127.0.0.1:${port} does not work: $(cat "${scratch}/ssh.err")"

# --- the remote fleetd: the binary under test, in SSH mode, as the remote user ---
tmpk="${scratch}/remote-kubeconfig"
itest_fake_kubeconfig "${tmpk}"
rkube="${rhome}/kube/config"
sudo -u "${ruser}" -H sh -c 'mkdir -p ~/.local/bin ~/.fleet ~/kube ~/opt-itest/bin && cat > ~/.fleet/config.json' <<'JSON'
{"remote_mcp_settings": {"fleet_enabled": true, "fleet_mode": "ssh"}}
JSON
sudo install -o "${ruser}" -m 0755 "${FLEET_BIN}" "${rhome}/.local/bin/fleet"
sudo install -o "${ruser}" -m 0600 "${tmpk}" "${rkube}"
info "start the remote fleetd (SSH mode) as ${ruser}"
sudo -u "${ruser}" -H env -u FLEET_SERVER -u FLEET_GATEWAY -u FLEET_SSH "${rhome}/.local/bin/fleet" ls >/dev/null
for _ in $(seq 1 "$(_scale_timeout 50)"); do
  sudo test -s "${rhome}/.fleet/ssh.port" && break
  sleep 0.2
done
sudo test -s "${rhome}/.fleet/ssh.port" || fail "the remote fleetd never opened its SSH listener"
rclusters() {
  sudo cat "${rhome}/.fleet/kube_clusters.json" \
    | perl -MJSON::PP -e 'local $/; my $j = decode_json(<STDIN>); my @c = @{$j->{clusters} || []}; '"$1"
}
local_clusters() {
  [ ! -s "${HOME}/.fleet/kube_clusters.json" ] || itest_kube_clusters 'print scalar @c'
}

# --- the local side: a fresh ~/.fleet, its fleetd started by a TUI on the SSH remote ---
pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
for _ in $(seq 1 "$(_scale_timeout 50)"); do
  [ "$(server_count)" = 0 ] && break
  sleep 0.2
done
[ "$(server_count)" = 0 ] || fail "a fleetd of ${FLEET_BIN} would not exit"
rm -rf "${HOME}/.fleet"
mkdir -p "${HOME}/.fleet"
# The tmux session would not inherit FLEET_SSH or PATH from this script; the
# fleetd the TUI starts inherits the TUI's PATH, so it runs the ssh wrapper.
remote_fleet="${scratch}/fleet-ssh"
cat > "${remote_fleet}" <<EOF
#!/bin/sh
export FLEET_SSH='ssh://${ruser}@127.0.0.1:${port}'
export PATH='${scratch}/bin':"\$PATH"
exec '${FLEET_BIN}' "\$@"
EOF
chmod +x "${remote_fleet}"
FLEET_BIN="${remote_fleet}" tui_spawn
tui_wait_for "No instances" 40
tui_wait_for "127.0.0.1" 10
tui_send Enter
tui_wait_for "Tmux vim keys" 10
tui_wait_for "contexts from fleetd" 10

info "the remote picker reads a kubeconfig on the remote fleetd's host"
tui_cursor_to '\+ Kubernetes Cluster'
tui_send Enter
tui_wait_for "Connect a Kubernetes cluster" 10
tui_send C-o
sleep 0.3
tui_send C-u
tui_send_text "${rkube}"
tui_send Enter
tui_wait_for "itest-plugin" 15
tui_assert_contains "${rkube}" "the picker must name the remote's kubeconfig it read"
tui_cursor_to 'itest-plugin'
tui_wait_for "exec: itest-login-plugin" 5
tui_assert_not_contains "[L] login here" "[L] runs the plugin on this machine: never offered over Armada SSH"
tui_cursor_to '\* itest-dead'

info "test & connect, then connect the failing context anyway"
tui_send Enter
tui_wait_for "connect anyway" 60
tui_send Enter
tui_wait_for_absent "Connect a Kubernetes cluster" 20
tui_wait_for "☸ itest-dead" 5
[ "$(rclusters 'print scalar @c')" = 1 ] || fail "the remote fleetd should hold one connection"
[ "$(rclusters 'print "$c[0]{context} $c[0]{kubeconfig_paths}[0]"')" = "itest-dead ${rkube}" ] \
  || fail "the remote connection must pin the context and the remote's file: $(rclusters 'print "$c[0]{context} @{$c[0]{kubeconfig_paths}}"')"
[ "$(local_clusters)" = "" ] || [ "$(local_clusters)" = 0 ] || fail "the local fleetd must hold no connection"

info "the remote editor sets the Storage pickers, Allow privileged and Extra PATH"
tui_cursor_to '☸ itest-dead'
tui_send Enter
tui_wait_for "Edit cluster ☸ itest-dead" 5
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
[ "$(rclusters 'print "$c[0]{shared_storage_class} $c[0]{storage_class}"')" = "itest-shared itest-disks" ] \
  || fail "the storage classes picked over Armada SSH were not saved: $(rclusters 'print "$c[0]{shared_storage_class} $c[0]{storage_class}"')"
tui_cursor_to '▶ Security'
tui_send l
tui_cursor_to 'Allow privileged:'
tui_send Right
tui_wait_for "Privileged allowed" 15
tui_cursor_to '▶ Advanced'
tui_send l
tui_cursor_to 'Extra PATH:'
tui_send Enter
sleep 0.3
tui_send_text "${rhome}/opt-itest/bin"
tui_send Enter
tui_wait_for "Saved Extra PATH" 15
[ "$(rclusters 'print $c[0]{allow_privileged} ? "on" : "off"')" = on ] || fail "allow_privileged set over Armada SSH was not saved"
[ "$(rclusters 'print join(":", @{$c[0]{extra_path}})')" = "${rhome}/opt-itest/bin" ] || fail "extra_path set over Armada SSH was not saved"
tui_send Escape
tui_wait_for_absent "Edit cluster" 5

info "and disconnects it"
tui_cursor_to '☸ itest-dead'
tui_send l
tui_send l
tui_send Enter
tui_wait_for "Disconnect ☸ itest-dead" 5
tui_send Enter
tui_wait_for "Disconnected ☸ itest-dead" 20
[ "$(rclusters 'print scalar @c')" = 0 ] || fail "the remote connection must be gone"

# The rest needs a cluster that answers: the kubernetes run's, else the
# current context when there is one (a QA container started with
# FLEET_QA_START_MINIKUBE=1).
kube_ctx="${FLEET_ITEST_CLUSTER:-$(kubectl config current-context 2>/dev/null || true)}"
if [ -z "${kube_ctx}" ] || ! kubectl --context "${kube_ctx}" --request-timeout=10s get --raw=/readyz >/dev/null 2>&1; then
  pass "a remote TUI over Armada SSH connects, edits (Storage pickers, Allow privileged, Extra PATH) and disconnects a cluster as a local one does (no reachable cluster: the remote's own kubeconfig half did not run)"
fi

kube_ns="fleet-it-203-$$"
kubectl --context "${kube_ctx}" create namespace "${kube_ns}" >/dev/null
info "on ${kube_ctx}: the remote connects it from its own ~/.kube/config into ${kube_ns}"
# The remote's default kubeconfig, credentials inlined (the invoking user's
# may point at files the remote user cannot read).
kubectl config view --raw --minify --flatten --context "${kube_ctx}" \
  | sudo -u "${ruser}" -H sh -c 'umask 077 && mkdir -p ~/.kube && cat > ~/.kube/config'
tui_cursor_to '\+ Kubernetes Cluster'
tui_send Enter
tui_wait_for "Connect a Kubernetes cluster" 10
tui_wait_for "${kube_ctx}" 15
tui_send /
tui_send_text "${kube_ctx}"
tui_send Escape
sleep 0.3
tui_send n
sleep 0.3
tui_send C-u
tui_send_text "${kube_ns}"
tui_send Enter
tui_wait_for "New connection ☸" 5
tui_wait_for "✓ exists" 180
tui_cursor_to '\[ (Test & connect|Connect) \]'
tui_send Enter
tui_wait_for_absent "Connect a Kubernetes cluster" 240
[ "$(rclusters 'print scalar @c')" = 1 ] || fail "the remote fleetd should hold the cluster's connection"
[ "$(rclusters 'print "$c[0]{context} $c[0]{namespace} $c[0]{kubeconfig_paths}[0]"')" = "${kube_ctx} ${kube_ns} ${rhome}/.kube/config" ] \
  || fail "the remote connection must pin the remote's own kubeconfig: $(rclusters 'print "$c[0]{context} $c[0]{namespace} @{$c[0]{kubeconfig_paths}}"')"
[ "$(local_clusters)" = "" ] || [ "$(local_clusters)" = 0 ] || fail "the local fleetd must hold no connection"
tui_wait_for "● ready" 180

info "disconnect it"
tui_cursor_to "☸ ${kube_ctx:0:7}"
tui_send l
tui_send l
tui_send Enter
tui_wait_for "Disconnect ☸" 5
tui_send Enter
tui_wait_for "Disconnected ☸" 30
[ "$(rclusters 'print scalar @c')" = 0 ] || fail "the remote connection must be gone"

pass "a remote TUI over Armada SSH connects, edits (Storage pickers, Allow privileged, Extra PATH) and disconnects a cluster as a local one does; ${kube_ctx} connected ready from the remote's own kubeconfig"
