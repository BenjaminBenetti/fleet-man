# shellcheck shell=bash
# Helpers for the VS Code Remote-SSH tests (source AFTER common.sh).
#
# `fleet code` writes ~/.ssh/fleet_config, which routes every *.fleet host
# through `fleet ssh-proxy`; the tests reach instances with `ssh -F` on that
# file, so ~/.ssh/config is never needed (and is kept as it was:
# itest_editor_isolate_ssh). A remote daemon is the same daemon reached
# through a fleet gateway (itest_editor_gateway_*), as in
# 300_gateway_remote_grpc.sh. A fake VS Code server (itest_editor_fake_vscode)
# stands in for the real one: its launcher records `--install-extension`, and
# its server listens from an executable inside the install directory, which
# is how fleet's ssh-server knows a VS Code connection.

ITEST_EDITOR_GW_PID=""
ITEST_EDITOR_GATEWAY_URL=""
ITEST_EDITOR_SSH_SAVED=""

# itest_editor_isolate_ssh — move ~/.ssh/config and ~/.ssh/fleet_config aside
# for the test; itest_editor_cleanup puts them back.
itest_editor_isolate_ssh() {
  ITEST_EDITOR_SSH_SAVED=$(mktemp -d)
  mkdir -p "${HOME}/.ssh"
  chmod 700 "${HOME}/.ssh"
  local f
  for f in config fleet_config; do
    if [ -e "${HOME}/.ssh/${f}" ] || [ -L "${HOME}/.ssh/${f}" ]; then
      mv "${HOME}/.ssh/${f}" "${ITEST_EDITOR_SSH_SAVED}/${f}"
    fi
  done
}

# itest_editor_cleanup — stop the gateway, restore ~/.ssh. Call it from
# itest_cleanup.
itest_editor_cleanup() {
  if [ -n "${ITEST_EDITOR_GW_PID}" ]; then
    kill "${ITEST_EDITOR_GW_PID}" 2>/dev/null || true
    wait "${ITEST_EDITOR_GW_PID}" 2>/dev/null || true
    ITEST_EDITOR_GW_PID=""
  fi
  if [ -n "${ITEST_EDITOR_SSH_SAVED}" ] && [ -d "${ITEST_EDITOR_SSH_SAVED}" ]; then
    local f
    for f in config fleet_config; do
      rm -f "${HOME}/.ssh/${f}"
      if [ -e "${ITEST_EDITOR_SSH_SAVED}/${f}" ] || [ -L "${ITEST_EDITOR_SSH_SAVED}/${f}" ]; then
        mv "${ITEST_EDITOR_SSH_SAVED}/${f}" "${HOME}/.ssh/${f}"
      fi
    done
    rm -rf "${ITEST_EDITOR_SSH_SAVED}"
    ITEST_EDITOR_SSH_SAVED=""
  fi
}

_itest_editor_free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

# itest_editor_gateway_start — configure remote fleet through a local
# gateway and start that gateway. Call before the daemon starts (fleet_up).
itest_editor_gateway_start() {
  local public grpc log up=false
  public=$(_itest_editor_free_port)
  grpc=$(_itest_editor_free_port)
  while [ "${grpc}" = "${public}" ]; do grpc=$(_itest_editor_free_port); done
  cat > "${HOME}/.fleet/config.json" <<EOF
{
  "remote_mcp_settings": {
    "enabled": true,
    "fleet_enabled": true,
    "gateway_url": "http://127.0.0.1:${grpc}"
  }
}
EOF
  itest_config_written
  log="${TEST_SCRATCH_DIR}/gateway.log"
  "${FLEET_BIN}" gateway \
    --public-url "http://127.0.0.1:${public}" \
    --public-grpc-url "http://127.0.0.1:${grpc}" \
    --public-addr "127.0.0.1:${public}" \
    --grpc-addr "127.0.0.1:${grpc}" \
    >"${log}" 2>&1 &
  ITEST_EDITOR_GW_PID=$!
  for _ in $(seq 1 "$(_scale_timeout 50)"); do
    if curl -fsS "http://127.0.0.1:${public}/healthz" >/dev/null 2>&1; then
      up=true
      break
    fi
    kill -0 "${ITEST_EDITOR_GW_PID}" 2>/dev/null || { cat "${log}" || true; fail "the gateway exited early"; }
    sleep 0.2
  done
  [ "${up}" = true ] || { cat "${log}" || true; fail "the gateway never came up"; }
}

# itest_editor_gateway_url — wait for the daemon to register with the
# gateway, then set ITEST_EDITOR_GATEWAY_URL to its FLEET_GATEWAY URL.
itest_editor_gateway_url() {
  local session="${HOME}/.fleet/gateway_session.json"
  for _ in $(seq 1 "$(_scale_timeout 150)"); do
    [ -f "${session}" ] && break
    sleep 0.2
  done
  [ -f "${session}" ] || fail "the daemon never registered with the gateway"
  ITEST_EDITOR_GATEWAY_URL=$(grep -oE '"public_grpc_url"[[:space:]]*:[[:space:]]*"[^"]+"' "${session}" | sed -E 's/.*"([^"]+)"$/\1/')
  [ -n "${ITEST_EDITOR_GATEWAY_URL}" ] || fail "no public_grpc_url in ${session}"
}

# itest_editor_ssh <ssh args...> — ssh through ~/.ssh/fleet_config from a
# shell with none of the remote-daemon variables.
itest_editor_ssh() {
  env -u FLEET_GATEWAY -u FLEET_TOKEN -u FLEET_SSH -u FLEET_SERVER \
    ssh -F "${HOME}/.ssh/fleet_config" -o BatchMode=yes -o LogLevel=ERROR \
    -o ConnectTimeout="$(_scale_timeout 120)" "$@"
}

# itest_editor_sftp <sftp args...> — sftp the same way.
itest_editor_sftp() {
  env -u FLEET_GATEWAY -u FLEET_TOKEN -u FLEET_SSH -u FLEET_SERVER \
    sftp -F "${HOME}/.ssh/fleet_config" -o BatchMode=yes -o LogLevel=ERROR "$@"
}

# itest_editor_uri_part <line> <alias|folder> — the alias, or the folder, of
# the vscode-remote://ssh-remote+<alias><folder> URI on a printed line.
itest_editor_uri_part() {
  local uri
  uri=$(printf '%s\n' "$1" | grep -oE 'vscode-remote://ssh-remote\+[^ '"'"']+' | head -1)
  [ -n "${uri}" ] || fail "no Remote-SSH URI in: $1"
  uri="${uri#vscode-remote://ssh-remote+}"
  case "$2" in
    alias) printf '%s' "${uri%%/*}" ;;
    folder) printf '/%s' "${uri#*/}" ;;
  esac
}

# itest_editor_fake_vscode <ref> <commit> — install a fake VS Code server in
# the instance at ~/.vscode-server/bin/<commit>: `bin/code-server
# --install-extension <id>` appends the id to ~/.vscode-server/fake-installs.log;
# `bin/code-server --start-server <port>` serves 127.0.0.1:<port> from
# `node` (a copy of perl inside the install), greeting each connection with
# "vscode-server" and holding it until the client closes.
itest_editor_fake_vscode() {
  local ref commit="$2" launcher
  ref=$(_itest_ref "$1")
  launcher="${TEST_SCRATCH_DIR}/fake-code-server"
  cat > "${launcher}" <<'EOF'
#!/bin/sh
here=$(cd "$(dirname "$0")/.." && pwd)
case "$1" in
  --install-extension)
    echo "$2" >> "$HOME/.vscode-server/fake-installs.log"
    mkdir -p "$HOME/.vscode-server/extensions/$2-0.0.1"
    echo "Extension '$2' v0.0.1 was successfully installed."
    ;;
  --start-server)
    exec "$here/node" -MIO::Socket::INET -e '
      my $s = IO::Socket::INET->new(LocalAddr => "127.0.0.1", LocalPort => $ARGV[0], Listen => 16, ReuseAddr => 1)
        or die "listen: $!\n";
      print STDERR "fake vscode server listening on $ARGV[0]\n";
      $SIG{CHLD} = "IGNORE";
      while (my $c = $s->accept) {
        if (!fork) { $c->autoflush(1); print $c "vscode-server\n"; while (<$c>) {} exit 0 }
        close $c;
      }' "$2"
    ;;
  *) echo "fake code-server: $*" >&2; exit 2 ;;
esac
EOF
  "${FLEET_BIN}" exec "${ref}" -- sh -c "d=\"\$HOME/.vscode-server/bin/${commit}\"; mkdir -p \"\$d/bin\" && cp \"\$(command -v perl)\" \"\$d/node\" && cat > \"\$d/bin/code-server\" && chmod +x \"\$d/bin/code-server\"" < "${launcher}"
}

# itest_editor_vscode_attach <alias> <commit> <port> <out> — what VS Code
# Remote-SSH does on a window's connect: a session starts the server (if it
# is not running), then a connection forwards to its port and stays open
# until itest_editor_vscode_detach. The forward's output goes to <out>.
ITEST_EDITOR_ATTACH_PIDS=""
itest_editor_vscode_attach() {
  local alias="$1" commit="$2" port="$3" out="$4" probe
  probe="perl -MIO::Socket::INET -e 'exit(IO::Socket::INET->new(PeerAddr => \"127.0.0.1:${port}\", Timeout => 2) ? 0 : 1)'"
  itest_editor_ssh "${alias}" "if ! ${probe}; then nohup \"\$HOME/.vscode-server/bin/${commit}/bin/code-server\" --start-server ${port} >/tmp/fake-vscode-${port}.log 2>&1 </dev/null & fi; for i in \$(seq 1 50); do ${probe} && exit 0; sleep 0.2; done; cat /tmp/fake-vscode-${port}.log; exit 1" \
    || fail "the fake VS Code server did not start in the instance"
  rm -f "${out}.in"
  mkfifo "${out}.in"
  itest_editor_ssh -W "127.0.0.1:${port}" "${alias}" < "${out}.in" > "${out}" 2>"${out}.err" &
  ITEST_EDITOR_ATTACH_PIDS="$!"
  # Hold the forward's stdin open: the connection lasts until the detach.
  sleep 3600 > "${out}.in" &
  ITEST_EDITOR_ATTACH_PIDS="${ITEST_EDITOR_ATTACH_PIDS} $!"
}

# itest_editor_vscode_detach — end the VS Code connection the last attach
# opened.
itest_editor_vscode_detach() {
  local p
  for p in ${ITEST_EDITOR_ATTACH_PIDS}; do kill "${p}" 2>/dev/null || true; done
  for p in ${ITEST_EDITOR_ATTACH_PIDS}; do wait "${p}" 2>/dev/null || true; done
  ITEST_EDITOR_ATTACH_PIDS=""
}
