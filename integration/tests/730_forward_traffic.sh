#!/usr/bin/env bash
# itest: k8s
# Description: `fleet port-forward add` carries bulk traffic both ways intact, keeps a client half-close (the peer still answers after EOF, and with --half-close also 3s later; on a cluster the uploads take --half-close forwards, as its port-forward drops the half-close), serves parallel connections, and a forward to a closed port fails alone.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
source "$(dirname "$0")/../net/net.sh"
workdir=$(mktemp -d)
forward_pid=""
late_pid=""
itest_cleanup() {
  [ -z "${forward_pid}" ] || kill "${forward_pid}" 2>/dev/null || true
  [ -z "${late_pid}" ] || kill "${late_pid}" 2>/dev/null || true
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
# on devcontainer its forward goes through the backend's in-container bridge
# instead (`docker exec socat`, which is why the fixture installs socat). On a
# cluster every forward reaches the pod's loopback (a stream pair on the pod's
# pooled port-forward).
itest_net_serve "${inst}" sink 17310
"${FLEET_BIN}" exec "${inst}" -- sh -c "head -c ${download_bytes} /dev/urandom > /tmp/itest-source.bin"
source_sha=$("${FLEET_BIN}" exec "${inst}" -- sha256sum /tmp/itest-source.bin | awk '{print $1}')
itest_net_serve "${inst}" source 17311 0.0.0.0 /tmp/itest-source.bin
itest_net_serve "${inst}" echo 17312
itest_net_serve "${inst}" sink 17313 127.0.0.1
itest_net_serve "${inst}" latesink 17314
# 17319: nothing listens there.

sink_port=$(itest_net_free_port)
source_port=$(itest_net_free_port)
echo_port=$(itest_net_free_port)
loop_port=$(itest_net_free_port)
late_port=$(itest_net_free_port)
closed_port=$(itest_net_free_port)
hc_sink_port=$(itest_net_free_port)
hc_loop_port=$(itest_net_free_port)
ports="${sink_port} ${source_port} ${echo_port} ${loop_port} ${late_port} ${closed_port} ${hc_sink_port} ${hc_loop_port}"

info "fleet port-forward add alpha (5 mappings), and --half-close ones (3)"
forward_log="${workdir}/forward.log"
"${FLEET_BIN}" port-forward add "${inst}" \
  "${sink_port}:17310" "${source_port}:17311" "${echo_port}:17312" \
  "${loop_port}:17313" "${closed_port}:17319" > "${forward_log}" 2>&1 &
forward_pid=$!
# A cluster's port-forward does not pass the local side's half-close on (the
# peer never reads EOF, and the runtime ends the connection about a second
# later); --half-close takes the pod agent's path, which keeps a true
# half-close. On devcontainer the direct dial keeps one either way.
late_log="${workdir}/late.log"
"${FLEET_BIN}" port-forward add --half-close "${inst}" \
  "${late_port}:17314" "${hc_sink_port}:17310" "${hc_loop_port}:17313" > "${late_log}" 2>&1 &
late_pid=$!
# The uploads (data, then a half-close, then the peer's reply) go through the
# forward that keeps a half-close on this backend.
upload_port="${sink_port}"
upload_loop_port="${loop_port}"
if itest_backend_is kubernetes; then
  upload_port="${hc_sink_port}"
  upload_loop_port="${hc_loop_port}"
fi
for port in ${ports}; do
  itest_net_wait_accepts "${port}" 30 || fail "local port ${port} never accepted: $(cat "${forward_log}" "${late_log}")"
done
assert_contains "$(cat "${forward_log}")" "Forwarding localhost:${sink_port} -> alpha:17310" "the forward should announce each mapping"
assert_contains "$(cat "${forward_log}")" "Press Ctrl+C to stop all forwards." "the forward should run in the foreground"
assert_contains "$(cat "${late_log}")" "Forwarding localhost:${late_port} -> alpha:17314" "the half-close forward should announce its mapping"

info "upload ${upload_bytes} bytes, half-close, and read the sink's reply"
out=$(itest_net_client upload "${upload_port}" "${upload_bytes}" 2>&1) || fail "${out}"
info "${out}"
if itest_backend_is kubernetes; then
  # Recorded, not required: the cluster's port-forward drops the half-close.
  out=$(itest_net_client upload "${sink_port}" 1048576 2>&1) || true
  info "the cluster's port-forward after a half-close: ${out}"
fi

info "a --half-close forward keeps the read side open: the reply comes 3s after EOF"
# Longer than a bridge that closes soon after the client's EOF allows (a
# cluster runtime's port-forward closes ~1s after it, a socat bridge 0.5s), so
# only a true half-close passes.
out=$(itest_net_client upload "${late_port}" 1048576 --late 3 2>&1) || fail "${out}"
info "${out}"

info "download ${download_bytes} bytes"
out=$(itest_net_client download "${source_port}" "${download_bytes}" "${source_sha}" 2>&1) || fail "${out}"
info "${out}"

info "stream 2 MiB while reading the echo back"
out=$(itest_net_client echo "${echo_port}" 2097152 2>&1) || fail "${out}"
info "${out}"

info "8 uploads in parallel on one forward"
out=$(itest_net_client upload "${upload_port}" 1048576 --parallel 8 2>&1) || fail "${out}"
info "${out}"

info "a loopback-only service (upload + half-close)"
# An immediate reply only: on devcontainer this goes through the in-container
# socat bridge, which ends 0.5s after the client's EOF (its default -t), so a
# late reply is lost on this path.
out=$(itest_net_client upload "${upload_loop_port}" 2097152 2>&1) || fail "${out}"
info "${out}"

info "a forward to a closed port closes each connection, and the other forwards keep working"
out=$(itest_net_client closed "${closed_port}" 2>&1) || fail "${out}"
info "${out}"
out=$(itest_net_client echo "${echo_port}" 65536 2>&1) || fail "after the refused connection: ${out}"
kill -0 "${forward_pid}" 2>/dev/null || fail "fleet port-forward exited after a refused connection: $(cat "${forward_log}")"

info "Ctrl+C stops every forward"
kill -INT "${forward_pid}" "${late_pid}"
wait "${forward_pid}" 2>/dev/null || true
wait "${late_pid}" 2>/dev/null || true
forward_pid=""
late_pid=""
assert_contains "$(cat "${forward_log}")" "Stopping port forwards..." "the forward should report stopping"
for port in ${ports}; do
  if itest_net_accepts "${port}"; then
    fail "local port ${port} still accepts after the forward stopped"
  fi
done

pass "port-forward traffic, half-close, parallel connections and a closed port"
