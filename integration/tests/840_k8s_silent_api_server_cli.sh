#!/usr/bin/env bash
# itest: k8s
# Description: on a connection reached through a local TCP proxy, an edit of the connection's deb cache image reaches the fleet's running cache Deployment; once the API server stops answering (the proxy takes connections and never forwards them), `fleet spawn-session` fails within its connection's bound saying why (not a bare exit status), and a connection removed while its cluster cannot be reached says what it leaves in the namespace.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
PROXY_PID=""
itest_cleanup() {
  [ -z "${PROXY_PID}" ] || kill "${PROXY_PID}" >/dev/null 2>&1 || true
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: a cluster whose API server stops answering"

setup_test
cat > "${HOME}/.fleet/state.json" <<EOF
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {
      "name": "${FIXTURE_REPO_NAME}",
      "remote": "${FIXTURE_REPO_URL}",
      "settings": { "debCacheServer": true },
      "instances": []
    }
  }
}
EOF

# --- the proxy: the API server, through a port this test can silence ---
server=$(kubectl config view --minify --context "${FLEET_ITEST_CLUSTER}" -o 'jsonpath={.clusters[0].cluster.server}')
hostport="${server#https://}"
hostport="${hostport%%/*}"
api_host="${hostport%:*}"
api_port="${hostport##*:}"
proxy_port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
# With "silent" it takes every connection and holds it, never forwarding a
# byte: an API server that is down behind an address that still answers.
cat > "${TEST_SCRATCH_DIR}/proxy.py" <<'PY'
import socket, sys, threading
port, host, upstream, mode = int(sys.argv[1]), sys.argv[2], int(sys.argv[3]), sys.argv[4]
srv = socket.socket()
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", port))
srv.listen(128)
held = []
def pipe(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d:
                break
            b.sendall(d)
    except OSError:
        pass
    finally:
        try:
            b.shutdown(socket.SHUT_WR)
        except OSError:
            pass
while True:
    c, _ = srv.accept()
    if mode == "silent":
        held.append(c)
        continue
    try:
        u = socket.create_connection((host, upstream))
    except OSError:
        c.close()
        continue
    threading.Thread(target=pipe, args=(c, u), daemon=True).start()
    threading.Thread(target=pipe, args=(u, c), daemon=True).start()
PY
start_proxy() {
  python3 "${TEST_SCRATCH_DIR}/proxy.py" "${proxy_port}" "${api_host}" "${api_port}" "$1" &
  PROXY_PID=$!
  for _ in $(seq 1 50); do
    if python3 -c "import socket,sys; socket.create_connection(('127.0.0.1', ${proxy_port}), 1)" 2>/dev/null; then return 0; fi
    sleep 0.1
  done
  fail "the proxy never listened"
}
start_proxy forward

cut_config="${TEST_SCRATCH_DIR}/kube-cut.yaml"
kubectl config view --minify --flatten --raw --context "${FLEET_ITEST_CLUSTER}" > "${cut_config}"
kubectl --kubeconfig "${cut_config}" config set-cluster "$(kubectl --kubeconfig "${cut_config}" config view -o 'jsonpath={.clusters[0].name}')" \
  --server "https://127.0.0.1:${proxy_port}" --tls-server-name "${api_host}" >/dev/null
out=$("${FLEET_BIN}" cluster add "${FLEET_ITEST_CLUSTER}" --name cut --kubeconfig "${cut_config}" -n "${FLEET_ITEST_NAMESPACE}" 2>&1) \
  || fail "cluster add through the proxy: ${out}"
"${FLEET_BIN}" cluster test cut >/dev/null || fail "the proxied connection does not test usable"
# A second connection through the proxy, reached once and holding no
# instance: the one removed while the API server is silent.
out=$("${FLEET_BIN}" cluster add "${FLEET_ITEST_CLUSTER}" --name ghost --kubeconfig "${cut_config}" -n default 2>&1) \
  || fail "cluster add ghost: ${out}"
"${FLEET_BIN}" cluster test ghost >/dev/null 2>&1 || true

inst="${FIXTURE_REPO_NAME}/beta"
info "fleet up beta on the proxied connection (deb cache on)"
"${FLEET_BIN}" up beta --repo "${FIXTURE_REPO_URL}" --cluster cut

# The fleet's deb cache Deployment on the proxied connection (the shared
# helpers look at the test's own connection), once it is up.
cut_id=$(perl -MJSON::PP -e 'local $/; my $j = decode_json(<STDIN>);
  for my $c (@{$j->{clusters}}) { if (($c->{display_name} // "") eq "cut") { print $c->{id}; exit 0 } }' < "${HOME}/.fleet/kube_clusters.json")
[ -n "${cut_id}" ] || fail "no connection named cut"
deploy=""
for _ in $(seq 1 "$(_scale_timeout 240)"); do
  deploy=$(itest_k8s_kubectl get deployments -l "app.kubernetes.io/managed-by=fleet,fleet.dev/cluster=${cut_id}" -o name 2>/dev/null \
    | sed -n 's#^deployment[^/]*/\(.*-aptcache\)$#\1#p' | head -n 1)
  if [ -n "${deploy}" ] && [ "$(itest_k8s_kubectl get deployment "${deploy}" -o 'jsonpath={.status.availableReplicas}' 2>/dev/null)" -ge 1 ] 2>/dev/null; then
    break
  fi
  sleep 1
done
[ -n "${deploy}" ] || fail "the fleet has no deb cache Deployment on the proxied connection"

# --- an edit of the cache's image reaches the running Deployment ---
image() { itest_k8s_kubectl get deployment "${deploy}" -o 'jsonpath={.spec.template.spec.containers[0].image}'; }
before=$(image)
edited="docker.io/sameersbn/apt-cacher-ng:latest@sha256:$(printf '0%.0s' $(seq 1 64))"
info "service_images.debcache: ${before} -> ${edited}"
out=$("${FLEET_BIN}" cluster set cut "service_images.debcache=${edited}" 2>&1) || fail "cluster set service_images: ${out}"
for _ in $(seq 1 "$(_scale_timeout 60)"); do
  [ "$(image)" = "${edited}" ] && break
  sleep 1
done
assert_equals "${edited}" "$(image)" "the running deb cache Deployment takes the connection's edited image"

info "spawn-session works through the proxy"
"${FLEET_BIN}" spawn-session "${inst}" before >/dev/null || fail "spawn-session before the API server went silent"

# --- the API server stops answering ---
info "silencing the API server"
kill "${PROXY_PID}"
wait "${PROXY_PID}" 2>/dev/null || true
PROXY_PID=""
start_proxy silent
silenced=$(date +%s)

start=$(date +%s)
rc=0
err=$("${FLEET_BIN}" spawn-session "${inst}" during 2>&1) || rc=$?
took=$(( $(date +%s) - start ))
info "spawn-session with a silent API server: exit ${rc} after ${took}s: ${err}"
[ "${rc}" -ne 0 ] || fail "spawn-session succeeded with the API server silent"
# The kernel's connect retries alone took over two minutes, and a held
# connection's TLS handshake had no end at all.
[ "${took}" -lt "$(_scale_timeout 100)" ] || fail "spawn-session took ${took}s to fail on a silent API server"
reason="${err#*failed to create session \"*\": }"
case "${reason}" in
  "exit status "*) fail "spawn-session gives only the exit status: ${err}" ;;
esac
assert_contains "${err}" "kubernetes:" "spawn-session names the cluster's error class"

# --- a connection removed while unreachable says what it leaves ---
# Its owner Lease lapses once it goes unrenewed for most of its 90s.
while [ $(( $(date +%s) - silenced )) -lt 80 ]; do sleep 2; done
out=$("${FLEET_BIN}" cluster rm ghost 2>&1) || fail "cluster rm ghost: ${out}"
info "cluster rm ghost: ${out}"
assert_contains "${out}" "warning: left in namespace default:" "the removal says what stays in the namespace"
assert_contains "${out}" "the cluster could not be reached" "the removal says why it could not delete it"
assert_contains "${out}" "fleet.dev/cluster=" "the removal names the labels to find it by"
assert_contains "${out}" "Removed cluster \"ghost\"" "the connection is removed"

info "healing the API server"
kill "${PROXY_PID}"
wait "${PROXY_PID}" 2>/dev/null || true
PROXY_PID=""
start_proxy forward

pass "a cache image edit reaches the running cache; a silent API server fails spawn-session in its bound, naming why; an unreachable connection's removal says what stays"
