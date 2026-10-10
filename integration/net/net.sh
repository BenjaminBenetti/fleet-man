# shellcheck shell=bash
# Network helpers (itest_net_*) for the port-forward, browser proxy and
# host-reachability itests. Source after common.sh:
#
#   source "$(dirname "$0")/../common.sh"
#   source "$(dirname "$0")/../net/net.sh"
#
# The in-instance end is integration/fixture-net/itest-net.pl (perl core only,
# so it runs on the stock devcontainer base image); the host end is
# integration/net/client.py. Both are backend-neutral: the server is started and
# probed through `fleet exec`, and the client only talks to host ports.

FIXTURE_NET_SRC="${INTEGRATION_DIR}/fixture-net"
ITEST_NET_CLIENT="${INTEGRATION_DIR}/net/client.py"
# Where the fixture's server script lands inside an instance (its checkout).
ITEST_NET_SERVER="/workspaces/${FIXTURE_REPO_NAME}/itest-net.pl"

# setup_net_test — setup_test on the network fixture (itest-net.pl, socat, and
# a Fleet Launch block whose sites and app point at itest-net.pl).
setup_net_test() {
  FIXTURE_SRC="${FIXTURE_NET_SRC}" setup_test
}

# itest_net_client <subcommand> <args...> — run net/client.py (see its usage).
itest_net_client() {
  python3 "${ITEST_NET_CLIENT}" "$@"
}

# itest_net_free_port — print a TCP port that is free on this host.
itest_net_free_port() {
  python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1]); s.close()'
}

# itest_net_accepts <port> — true when something on this host accepts TCP
# connections on 127.0.0.1:<port>.
itest_net_accepts() {
  python3 -c 'import socket, sys; socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=2).close()' "$1" 2>/dev/null
}

# itest_net_wait_accepts <port> [timeout_s] — wait until 127.0.0.1:<port>
# accepts; returns 1 on timeout.
itest_net_wait_accepts() {
  local port="$1" deadline
  deadline=$(( $(date +%s) + $(_scale_timeout "${2:-30}") ))
  until itest_net_accepts "${port}"; do
    [ "$(date +%s)" -lt "${deadline}" ] || return 1
    sleep 0.25
  done
}

# itest_net_serve <ref> <mode> <port> [bind] [file] — start itest-net.pl
# detached inside the instance, then wait until it is listening there. Its log
# is /tmp/itest-net-<port>.log in the instance; a server that cannot listen
# fails the test with that log.
itest_net_serve() {
  local ref mode="$2" port="$3" bind="${4:-0.0.0.0}" file="${5:-}" log deadline
  ref=$(_itest_ref "$1")
  log="/tmp/itest-net-${port}.log"
  "${FLEET_BIN}" exec "${ref}" -- sh -c \
    "nohup perl '${ITEST_NET_SERVER}' ${mode} ${port} ${bind} ${file} >'${log}' 2>&1 </dev/null &"
  deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
  until "${FLEET_BIN}" exec "${ref}" -- grep -q 'listening on' "${log}" 2>/dev/null; do
    if [ "$(date +%s)" -ge "${deadline}" ]; then
      fail "itest-net.pl ${mode} on ${bind}:${port} never listened in ${ref}: $("${FLEET_BIN}" exec "${ref}" -- cat "${log}" 2>&1 || true)"
    fi
    sleep 0.5
  done
}
