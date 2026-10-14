#!/usr/bin/env bash
# itest: k8s
# Description: on a minikube connection set to Gateway delivery, an instance's built image reaches the node through a local fleet gateway (TLS from a test CA the node trusts) and the instance runs; with fleetd running, the gateway stopped before a fresh pull makes the start fail with the classified kubernetes:image_pull error.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
GW_PID=""
TRUSTED=""
itest_cleanup() {
  stop_gateway
  if [ -n "${TRUSTED}" ]; then
    minikube -p "${PROFILE}" ssh -- "sudo rm -rf ${TRUSTED}" >/dev/null 2>&1 || true
  fi
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: Gateway image delivery"
PROFILE="${FLEET_ITEST_CLUSTER}"
command -v minikube >/dev/null 2>&1 && minikube -p "${PROFILE}" status >/dev/null 2>&1 \
  || pass "the run's cluster is not a minikube profile: the node's trust of the test CA is set through minikube ssh"

stop_gateway() {
  if [ -n "${GW_PID}" ]; then
    kill "${GW_PID}" 2>/dev/null || true
    wait "${GW_PID}" 2>/dev/null || true
    GW_PID=""
  fi
}
free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

setup_test

# --- a TLS gateway the node reaches as host.minikube.internal ---
host=host.minikube.internal
PUBLIC_PORT="$(free_port)"
GRPC_PORT="$(free_port)"
while [ "${GRPC_PORT}" = "${PUBLIC_PORT}" ]; do GRPC_PORT="$(free_port)"; done
tls="${TEST_SCRATCH_DIR}/tls"
mkdir -p "${tls}"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=fleet itest CA" \
  -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign,cRLSign \
  -keyout "${tls}/ca.key" -out "${tls}/ca.crt" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=${host}" \
  -keyout "${tls}/gw.key" -out "${tls}/gw.csr" 2>/dev/null
printf 'subjectAltName=DNS:%s,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' "${host}" > "${tls}/ext"
openssl x509 -req -in "${tls}/gw.csr" -CA "${tls}/ca.crt" -CAkey "${tls}/ca.key" -CAcreateserial -days 1 \
  -extfile "${tls}/ext" -out "${tls}/gw.crt" 2>/dev/null

# The node trusts the CA for the gateway's registry host (containerd's and
# docker's per-registry trust directories, whichever runtime the node has).
reg="${host}:${PUBLIC_PORT}"
TRUSTED="/etc/containerd/certs.d/${reg} /etc/docker/certs.d/${reg}"
ca_b64=$(base64 -w0 "${tls}/ca.crt")
minikube -p "${PROFILE}" ssh -- "for d in ${TRUSTED}; do sudo mkdir -p \$d && echo ${ca_b64} | base64 -d | sudo tee \$d/ca.crt >/dev/null; done" \
  || fail "could not install the test CA on the node"

gw_log="${TEST_SCRATCH_DIR}/gateway.log"
"${FLEET_BIN}" gateway --public-url "https://${reg}" --public-addr "0.0.0.0:${PUBLIC_PORT}" \
  --grpc-addr "127.0.0.1:${GRPC_PORT}" --session-key itest-807 \
  --tls-cert "${tls}/gw.crt" --tls-key "${tls}/gw.key" >"${gw_log}" 2>&1 &
GW_PID=$!
up=""
for _ in $(seq 1 "$(_scale_timeout 50)"); do
  if curl -fsS --cacert "${tls}/ca.crt" --resolve "${reg}:127.0.0.1" "https://${reg}/healthz" >/dev/null 2>&1; then up=1; break; fi
  kill -0 "${GW_PID}" 2>/dev/null || { cat "${gw_log}"; fail "the gateway exited early"; }
  sleep 0.2
done
[ -n "${up}" ] || { cat "${gw_log}"; fail "the gateway never answered /healthz"; }
assert_contains "$(cat "${gw_log}")" "image registry route on" "the gateway offers the image route"

# fleetd trusts the test CA (the daemon host's trust store, plus the CA) and
# serves its images through the gateway.
bundle="${tls}/bundle.crt"
cat /etc/ssl/certs/ca-certificates.crt "${tls}/ca.crt" > "${bundle}" 2>/dev/null || cp "${tls}/ca.crt" "${bundle}"
export SSL_CERT_FILE="${bundle}"
# The daemon gives a pull that cannot reach the gateway this long (not the
# default 15 minutes) before the start fails with the classified error.
export FLEET_KUBE_IMAGE_PULL_TIMEOUT=90s
cat > "${HOME}/.fleet/config.json" <<EOF
{
  "remote_mcp_settings": {
    "gateway_url": "https://127.0.0.1:${GRPC_PORT}",
    "image_registry_enabled": true
  }
}
EOF
itest_config_written
"${FLEET_BIN}" cluster set itest image.mode=gateway >/dev/null
session_file="${HOME}/.fleet/gateway_session.json"
for _ in $(seq 1 "$(_scale_timeout 150)"); do
  grep -q '"public_registry"' "${session_file}" 2>/dev/null && break
  sleep 0.2
done
grep -q '"public_registry"' "${session_file}" 2>/dev/null \
  || { tail -n 40 "${HOME}/.fleet/fleet.log"; fail "fleetd never registered for the image registry"; }

# --- an instance whose image is built on the daemon host ---
cat >"${FIXTURE_REPO_DIR}/.devcontainer/Dockerfile" <<'DOCKERFILE'
FROM mcr.microsoft.com/devcontainers/base:debian-12
RUN echo through-the-gateway > /etc/itest-gateway-image
DOCKERFILE
cat >"${FIXTURE_REPO_DIR}/.devcontainer/devcontainer.json" <<'JSON'
{
  "name": "fleet-man gateway fixture",
  "build": { "dockerfile": "Dockerfile" },
  "remoteUser": "vscode"
}
JSON
(cd "${FIXTURE_REPO_DIR}" && git add -A && git commit -qm "fixture: built image")

inst="${FIXTURE_REPO_NAME}/alpha"
info "fleet up alpha (built, delivered through the gateway)"
fleet_up alpha
log="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-alpha.log"
grep -E '^==> delivering image' "${log}" || true
cid=$(itest_container_id "${inst}")
pod=$(itest_k8s_pod "${cid}")
image=$(itest_k8s_kubectl get pod "${pod}" -o 'jsonpath={.spec.containers[?(@.name=="dev")].image}')
info "the pod runs ${image}"
case "${image}" in
  "${reg}/"*) ;;
  *) fail "the instance's image ${image} is not served through the gateway (${reg})" ;;
esac
assert_equals "through-the-gateway" "$("${FLEET_BIN}" exec "${inst}" -- cat /etc/itest-gateway-image)" "the built image runs"

# --- the gateway stopped before a fresh pull: the classified error ---
info "stopping the gateway, then a start that must pull the image afresh"
"${FLEET_BIN}" stop "${inst}" >/dev/null
stop_gateway
image_id="${image#*@}"
# Every way the node may hold it: crictl's answer is no proof on a docker
# runtime node (cri-dockerd), where only docker rmi drops a digest pull.
minikube -p "${PROFILE}" ssh -- "sudo crictl rmi ${image} >/dev/null 2>&1; sudo docker rmi -f ${image} >/dev/null 2>&1; sudo crictl rmi ${image_id} >/dev/null 2>&1; true"
rc=0
out=$("${FLEET_BIN}" start "${inst}" 2>&1) || rc=$?
printf '%s\n' "${out}"
[ "${rc}" -ne 0 ] || fail "the start pulled the image with the gateway stopped"
assert_contains "${out}" "kubernetes:image_pull" "the start's error code"
assert_contains "${out}" "could not reach the gateway at ${reg}" "the error names the unreachable gateway"

pass "a built image reaches the node through the gateway; with the gateway stopped the pull fails classified"
