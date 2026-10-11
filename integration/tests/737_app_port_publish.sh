#!/usr/bin/env bash
# itest: k8s
# Description: a devcontainer's appPort publishes the instance's ports on the daemon host while it runs: a numbered port on 127.0.0.1, a mapping without an address on 127.0.0.1 on a cluster (docker's 0.0.0.0 on devcontainer), and one naming 0.0.0.0 on every interface; a stop closes them and a start opens them again.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
source "$(dirname "$0")/../net/net.sh"
itest_begin

setup_net_test
# appPort: 17380 the CLI publishes as 127.0.0.1:17380; "17384:17381" names no
# address (docker: 0.0.0.0, a cluster: 127.0.0.1); "0.0.0.0:17385:17382" names
# every interface.
perl -0pi -e 's/("image": [^\n]*\n)/$1  "appPort": [17380, "17384:17381", "0.0.0.0:17385:17382"],\n/' \
  "${FIXTURE_REPO_DIR}/.devcontainer/devcontainer.json"
grep -q '"appPort"' "${FIXTURE_REPO_DIR}/.devcontainer/devcontainer.json" || fail "the fixture edit did not land"
(cd "${FIXTURE_REPO_DIR}" && git commit -qam "fixture: appPort")
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

for port in 17380 17381 17382; do
  itest_net_serve "${inst}" http "${port}"
done

# A non-loopback address of this host, where only a wildcard listener answers.
host_ip=$(python3 -c 'import socket; s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.connect(("192.0.2.1", 9)); print(s.getsockname()[0])')
[ -n "${host_ip}" ] && [ "${host_ip}" != "127.0.0.1" ] || fail "no non-loopback address on this host (${host_ip})"
info "this host's own address: ${host_ip}"

# get <host> <port> — the instance server's banner through a published port.
get() { curl -sS --max-time 10 "http://$1:$2/" 2>/dev/null || true; }

# wait_get <host> <port> <want> — until the published port answers want.
wait_get() {
  local deadline
  deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
  until [ "$(get "$1" "$2")" = "$3" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "$1:$2 never answered '$3' (got '$(get "$1" "$2")')"
    sleep 0.5
  done
}

info "the published ports answer on this host"
wait_get 127.0.0.1 17380 "fleet-itest-web 17380"
wait_get 127.0.0.1 17384 "fleet-itest-web 17381"
wait_get 127.0.0.1 17385 "fleet-itest-web 17382"
wait_get "${host_ip}" 17385 "fleet-itest-web 17382"

info "a port the CLI publishes on 127.0.0.1 is not on the host's network"
[ -z "$(get "${host_ip}" 17380)" ] || fail "17380 answers on ${host_ip}: it should be published on 127.0.0.1 only"
if itest_backend_is kubernetes; then
  # A cluster's default is 127.0.0.1: a remote daemon would otherwise publish
  # the pod on its own network.
  [ -z "$(get "${host_ip}" 17384)" ] || fail "17384 answers on ${host_ip}: a mapping without an address should stay on 127.0.0.1 on a cluster"
else
  # docker's default for a mapping without an address: every interface.
  wait_get "${host_ip}" 17384 "fleet-itest-web 17381"
fi

info "a stop closes the published ports"
"${FLEET_BIN}" stop "${inst}"
deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
while itest_net_accepts 17380 || itest_net_accepts 17384; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "a published port still accepts after the stop"
  sleep 0.5
done

info "a start publishes them again"
"${FLEET_BIN}" start "${inst}"
for port in 17380 17381; do
  itest_net_serve "${inst}" http "${port}"
done
wait_get 127.0.0.1 17380 "fleet-itest-web 17380"
wait_get 127.0.0.1 17384 "fleet-itest-web 17381"

pass "appPort publishes on the daemon host"
