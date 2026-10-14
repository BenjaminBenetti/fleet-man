#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster reached through a local TCP proxy, a 60s cut to the API server leaves an open TUI pane, a plain `o` shell and a running `fleet exec` intact once it heals: the exec finishes with all its output and exit code, and the pane and the shell run new commands; a pod deleted behind fleet's back then gets the pane a fresh session.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
PROXY_PID=""
EXEC_PID=""
itest_cleanup() {
  tui_kill
  [ -z "${EXEC_PID}" ] || kill "${EXEC_PID}" >/dev/null 2>&1 || true
  [ -z "${PROXY_PID}" ] || kill "${PROXY_PID}" >/dev/null 2>&1 || true
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: attaches through a cut to the API server"

setup_test

# --- the proxy: the API server, through a port this test can cut ---
server=$(kubectl config view --minify --context "${FLEET_ITEST_CLUSTER}" -o 'jsonpath={.clusters[0].cluster.server}')
hostport="${server#https://}"
hostport="${hostport%%/*}"
api_host="${hostport%:*}"
api_port="${hostport##*:}"
proxy_port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
cat > "${TEST_SCRATCH_DIR}/proxy.py" <<'PY'
import socket, sys, threading
port, host, upstream = int(sys.argv[1]), sys.argv[2], int(sys.argv[3])
srv = socket.socket()
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", port))
srv.listen(128)
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
    try:
        u = socket.create_connection((host, upstream))
    except OSError:
        c.close()
        continue
    threading.Thread(target=pipe, args=(c, u), daemon=True).start()
    threading.Thread(target=pipe, args=(u, c), daemon=True).start()
PY
start_proxy() {
  python3 "${TEST_SCRATCH_DIR}/proxy.py" "${proxy_port}" "${api_host}" "${api_port}" &
  PROXY_PID=$!
  for _ in $(seq 1 50); do
    if python3 -c "import socket,sys; socket.create_connection(('127.0.0.1', ${proxy_port}), 1)" 2>/dev/null; then return 0; fi
    sleep 0.1
  done
  fail "the proxy never listened"
}
start_proxy

# A kubeconfig of the same context with its server the proxy (the API
# server's certificate still checked against its own name).
cut_config="${TEST_SCRATCH_DIR}/kube-cut.yaml"
kubectl config view --minify --flatten --raw --context "${FLEET_ITEST_CLUSTER}" > "${cut_config}"
kubectl --kubeconfig "${cut_config}" config set-cluster "$(kubectl --kubeconfig "${cut_config}" config view -o 'jsonpath={.clusters[0].name}')" \
  --server "https://127.0.0.1:${proxy_port}" --tls-server-name "${api_host}" >/dev/null
out=$("${FLEET_BIN}" cluster add "${FLEET_ITEST_CLUSTER}" --name cut --kubeconfig "${cut_config}" -n "${FLEET_ITEST_NAMESPACE}" 2>&1) \
  || fail "cluster add through the proxy: ${out}"
"${FLEET_BIN}" cluster test cut >/dev/null || fail "the proxied connection does not test usable"

inst="${FIXTURE_REPO_NAME}/beta"
info "fleet up beta on the proxied connection"
"${FLEET_BIN}" up beta --repo "${FIXTURE_REPO_URL}" --cluster cut

# type_until <target> <line> <file> <needle> — type line (idempotent) into a
# tmux pane until it has written needle into file in the instance.
type_until() {
  local target="$1" line="$2" file="$3" needle="$4" next=0
  local deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    if [ "$(date +%s)" -ge "${next}" ]; then
      tmux send-keys -t "${target}" -l "${line}"
      tmux send-keys -t "${target}" Enter
      next=$(( $(date +%s) + 5 ))
    fi
    sleep 1
    "${FLEET_BIN}" exec "${inst}" -- cat "${file}" 2>/dev/null | grep -qF "${needle}" && return 0
  done
  printf -- '--- %s ---\n%s\n--- end ---\n' "${target}" "$(tmux capture-pane -t "${target}" -p 2>/dev/null || true)" >&2
  return 1
}

# --- a running fleet exec ---
exec_out="${TEST_SCRATCH_DIR}/exec.out"
"${FLEET_BIN}" exec "${inst}" -- sh -c 'i=0; while [ $i -lt 100 ]; do echo tick $i; i=$((i+1)); sleep 1; done; echo exec-done; exit 7' >"${exec_out}" 2>&1 &
EXEC_PID=$!

# --- a TUI pane and a plain `o` shell (a stub terminal, as in 762) ---
stub_dir="${TEST_SCRATCH_DIR}/terminal-stub"
mkdir -p "${stub_dir}"
cat > "${stub_dir}/ptyxis" <<EOF
#!/usr/bin/env bash
[ "\${1:-}" = "--" ] && shift
exec tmux new-window -d -t "${TUI_SESSION}:" -n ext "\$@"
EOF
chmod +x "${stub_dir}/ptyxis"
export PATH="${stub_dir}:${PATH}"
tui_spawn
tui_wait_for "beta" 30 || fail "the TUI never listed beta"
tui_wait_for "○ idle" 90 || fail "beta never showed running"
tui_cursor_to "▶ ○ beta"
tui_send o
tui_wait_for "Opened terminal for beta" 10 || fail "o did not open a terminal"
tui_send Enter
tui_wait_for_pane 2 20 || fail "enter did not open a pane on beta"
type_until "${TUI_SESSION}:0.1" 'echo pane-before-$((40+2)) >> /tmp/pane-marker' /tmp/pane-marker pane-before-42 \
  || fail "the pane did not run a command before the cut"
type_until "${TUI_SESSION}:ext" 'echo shell-before-$((40+2)) >> /tmp/shell-marker' /tmp/shell-marker shell-before-42 \
  || fail "the o shell did not run a command before the cut"

# --- the cut ---
info "cutting the API server off for 60s"
kill "${PROXY_PID}"
wait "${PROXY_PID}" 2>/dev/null || true
PROXY_PID=""
sleep 60
info "healing the cut"
start_proxy

# --- what came back ---
info "the fleet exec finishes"
rc=0
wait "${EXEC_PID}" || rc=$?
EXEC_PID=""
exec_text=$(cat "${exec_out}")
# `fleet exec` exits 1 for any non-zero exit inside, and says which.
if [ "${rc}" -eq 0 ] || ! printf '%s' "${exec_text}" | grep -qF "exit status 7"; then
  printf -- '--- fleet exec output ---\n%s\n--- fleet.log (kube) ---\n' "$(tail -n 20 "${exec_out}")"
  grep -iE 'kube|agent|exec' "${HOME}/.fleet/fleet.log" | tail -n 60 || true
  fail "the fleet exec's exit code across the cut: rc ${rc}, want the command's 7"
fi
assert_contains "${exec_text}" "exec-done" "the fleet exec ran to its end"
for n in 0 50 99; do
  assert_contains "${exec_text}" "tick ${n}" "the fleet exec's output kept tick ${n}"
done

info "the pane and the o shell run new commands"
type_until "${TUI_SESSION}:0.1" 'echo pane-after-$((40+2)) >> /tmp/pane-marker' /tmp/pane-marker pane-after-42 \
  || fail "the pane did not come back after the cut"
type_until "${TUI_SESSION}:ext" 'echo shell-after-$((40+2)) >> /tmp/shell-marker' /tmp/shell-marker shell-after-42 \
  || fail "the o shell did not come back after the cut"

# --- a pod deleted behind fleet's back: the pane rejoins a fresh session ---
cid=$(itest_container_id "${inst}")
pod=$(itest_k8s_pod "${cid}")
old_uid=$(itest_k8s_wait_pod_running "${pod}")
info "deleting ${pod}"
itest_k8s_kubectl delete pod "${pod}" --wait=false >/dev/null
itest_k8s_wait_pod_running "${pod}" "${old_uid}" >/dev/null
# fleetd sees the restart (the row's "sessions reset" notice is cleared at
# once by the pane's own re-attach, so the event log is where it shows).
for _ in $(seq 1 60); do
  grep -q 'the cluster restarted an instance.*cause="pod deleted"' "${HOME}/.fleet/fleet.log" && break
  sleep 1
done
grep -q 'the cluster restarted an instance.*cause="pod deleted"' "${HOME}/.fleet/fleet.log" \
  || fail "fleetd did not notice the pod was replaced"
type_until "${TUI_SESSION}:0.1" 'echo pane-rejoined-$((40+2)) >> /tmp/pane-marker' /tmp/pane-marker pane-rejoined-42 \
  || fail "the pane did not rejoin a fresh session after the pod was replaced"

pass "a 60s API cut leaves the pane, the o shell and fleet exec intact; a replaced pod's pane rejoins"
