#!/usr/bin/env bash
# Description: `fleet port-forward add` carries bulk traffic both ways intact, keeps a client half-close (the peer still answers after EOF), serves parallel connections, and a forward to a closed port fails alone.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
source "$(dirname "$0")/../net/net.sh"
workdir=$(mktemp -d)
forward_pid=""
itest_cleanup() {
  [ -z "${forward_pid}" ] || kill "${forward_pid}" 2>/dev/null || true
  rm -rf "${workdir}"
}
itest_begin

setup_net_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

# Payloads well past one Forward frame (32 KiB) and gRPC's 4 MB message
# ceiling, so chunking and flow control both get exercised.
upload_bytes=8388608
download_bytes=8388608

info "start the in-instance servers"
# Bound to every interface: on Linux the daemon dials the container's address
# directly. 17313 listens on loopback only, which that dial cannot reach, so
# its forward goes through the backend's in-container bridge instead
# (devcontainer: `docker exec socat`, which is why the fixture installs socat).
itest_net_serve "${inst}" sink 17310
"${FLEET_BIN}" exec "${inst}" -- sh -c "head -c ${download_bytes} /dev/urandom > /tmp/itest-source.bin"
source_sha=$("${FLEET_BIN}" exec "${inst}" -- sha256sum /tmp/itest-source.bin | awk '{print $1}')
itest_net_serve "${inst}" source 17311 0.0.0.0 /tmp/itest-source.bin
itest_net_serve "${inst}" echo 17312
itest_net_serve "${inst}" sink 17313 127.0.0.1
# 17319: nothing listens there.

sink_port=$(itest_net_free_port)
source_port=$(itest_net_free_port)
echo_port=$(itest_net_free_port)
loop_port=$(itest_net_free_port)
closed_port=$(itest_net_free_port)

info "fleet port-forward add alpha (5 mappings)"
forward_log="${workdir}/forward.log"
"${FLEET_BIN}" port-forward add "${inst}" \
  "${sink_port}:17310" "${source_port}:17311" "${echo_port}:17312" \
  "${loop_port}:17313" "${closed_port}:17319" > "${forward_log}" 2>&1 &
forward_pid=$!
for port in "${sink_port}" "${source_port}" "${echo_port}" "${loop_port}" "${closed_port}"; do
  itest_net_wait_accepts "${port}" 30 || fail "local port ${port} never accepted: $(cat "${forward_log}")"
done
assert_contains "$(cat "${forward_log}")" "Forwarding localhost:${sink_port} -> alpha:17310" "the forward should announce each mapping"
assert_contains "$(cat "${forward_log}")" "Press Ctrl+C to stop all forwards." "the forward should run in the foreground"

info "upload ${upload_bytes} bytes, half-close, and read the sink's reply"
out=$(itest_net_client upload "${sink_port}" "${upload_bytes}" 2>&1) || fail "${out}"
info "${out}"

info "download ${download_bytes} bytes"
out=$(itest_net_client download "${source_port}" "${download_bytes}" "${source_sha}" 2>&1) || fail "${out}"
info "${out}"

info "stream 2 MiB while reading the echo back"
out=$(itest_net_client echo "${echo_port}" 2097152 2>&1) || fail "${out}"
info "${out}"

info "8 uploads in parallel on one forward"
out=$(itest_net_client upload "${sink_port}" 1048576 --parallel 8 2>&1) || fail "${out}"
info "${out}"

info "a loopback-only service, through the in-container bridge (upload + half-close)"
out=$(itest_net_client upload "${loop_port}" 2097152 2>&1) || fail "${out}"
info "${out}"

info "a forward to a closed port closes each connection, and the other forwards keep working"
out=$(itest_net_client closed "${closed_port}" 2>&1) || fail "${out}"
info "${out}"
out=$(itest_net_client echo "${echo_port}" 65536 2>&1) || fail "after the refused connection: ${out}"
kill -0 "${forward_pid}" 2>/dev/null || fail "fleet port-forward exited after a refused connection: $(cat "${forward_log}")"

info "Ctrl+C stops every forward"
kill -INT "${forward_pid}"
wait "${forward_pid}" 2>/dev/null || true
forward_pid=""
assert_contains "$(cat "${forward_log}")" "Stopping port forwards..." "the forward should report stopping"
for port in "${sink_port}" "${source_port}" "${echo_port}" "${loop_port}" "${closed_port}"; do
  if itest_net_accepts "${port}"; then
    fail "local port ${port} still accepts after the forward stopped"
  fi
done

pass "port-forward traffic, half-close, parallel connections and a closed port"
