#!/usr/bin/env bash
# Description: enabling buildkit_server boots the fleet's shared buildkit server and gives the instance what reaches it: on docker a bind-mounted buildkitd socket, on a cluster its own client certificate (CN its key, ~/.fleet/buildkit) for the server's mutual TLS on its Service's port.
# itest: k8s
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
# claude=false codex=false homedir=/home/vscode gh=false buildkit=true
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/vscode false true

inst="${FIXTURE_REPO_NAME}/alpha"
in_instance() { "${FLEET_BIN}" exec "${inst}" -- "$@"; }

info "fleet up alpha (buildkit enabled)"
fleet_up alpha

# 1. The shared server is up. (fleet up blocks through the docker run + image
#    pull on docker; a cluster's Deployment comes up on the cluster's time.)
info "asserting the shared buildkit server is running"
itest_wait_service "${FIXTURE_REPO_NAME}" buildkit running 240

if itest_backend_is kubernetes; then
  deploy=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" deployments buildkit)
  secret=$(itest_k8s_fleet_object "${FIXTURE_REPO_NAME}" secrets buildkit)
  [ -n "${secret}" ] || fail "the fleet has no buildkit TLS Secret: $(itest_k8s_fleet_objects "${FIXTURE_REPO_NAME}" secrets | tr '\n' ' ')"

  # 2. It takes only mutual TLS on its Service's port: buildkitd requires a
  #    client certificate of the CA its Secret holds.
  args=$(itest_k8s_kubectl get deployment "${deploy}" -o 'jsonpath={.spec.template.spec.containers[0].args}')
  for want in "tcp://0.0.0.0:1234" "--tlscacert" "--tlscert" "--tlskey"; do
    assert_contains "${args}" "${want}" "the buildkit server's flags"
  done
  port=$(itest_k8s_kubectl get service "${deploy}" -o 'jsonpath={.spec.ports[0].port}')
  assert_equals "1234" "${port}" "the buildkit Service's port"

  # 3. The server is serving: buildctl in its own pod lists a worker.
  info "asserting the buildkit server answers buildctl debug workers"
  workers=$(itest_k8s_kubectl exec "deployment/${deploy}" -c buildkit -- buildctl debug workers 2>&1) \
    || fail "buildctl debug workers failed: ${workers}"
  assert_contains "${workers}" "linux/" "buildkit server reported no usable worker platform"

  # 4. The instance holds its own client certificate, the key its user's
  #    alone; fleetd issued it to this instance (CN its key) and logged it.
  info "asserting the instance holds its own client certificate"
  modes=$(in_instance sh -c 'd="$HOME/.fleet/buildkit"; stat -c "%a %n" "$d" "$d/ca.pem" "$d/cert.pem" "$d/key.pem"') \
    || fail "the instance has no buildkit credential: ${modes}"
  assert_contains "${modes}" "700 " "the credential folder is the user's alone"
  assert_contains "${modes}" "600 $(in_instance sh -c 'printf %s "$HOME"')/.fleet/buildkit/key.pem" "the key is the user's alone"
  key=$(itest_k8s_key "${inst}")
  issued=$(grep "issued an instance's client certificate" "${HOME}/.fleet/fleet.log" || true)
  assert_contains "${issued}" "cn=${key}" "fleetd logs the certificate it issued with the instance's key as its CN"
else
  bk_container="fleet-${FIXTURE_REPO_NAME}-buildkit"
  host_bk_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/.buildkit"
  host_sock="${host_bk_dir}/buildkitd.sock"

  # 2. The buildkitd socket and the bound cache dir exist on the host.
  info "asserting host socket + cache dir exist under .buildkit/"
  for _ in $(seq 1 "$(_scale_timeout 15)"); do
    [ -S "${host_sock}" ] && break
    sleep 1
  done
  [ -S "${host_sock}" ] || fail "host buildkitd.sock missing or not a socket: ${host_sock}"
  assert_file_exists "${host_bk_dir}/cache"

  # 3. The server is actually serving: buildctl (shipped in the moby/buildkit
  #    image) lists at least one worker over the unix socket.
  info "asserting the buildkit server answers buildctl debug workers"
  workers=$(docker exec "${bk_container}" \
    buildctl --addr unix:///run/fleet-buildkit/buildkitd.sock debug workers 2>&1) \
    || fail "buildctl debug workers failed: ${workers}"
  assert_contains "${workers}" "linux/" "buildkit server reported no usable worker platform"

  # 4. The socket is bind-mounted into the instance at the shared path.
  info "asserting the socket is bind-mounted into the instance"
  in_instance test -S /run/fleet-buildkit/buildkitd.sock \
    || fail "buildkitd.sock is not available inside the instance"
  mountinfo=$(in_instance cat /proc/self/mountinfo)
  assert_contains "${mountinfo}" " /run/fleet-buildkit " "/run/fleet-buildkit is not a mount point in the instance"
fi

# 5. The debian fixture has no docker/buildx, so ConfigureInstanceBuildx is a
#    silent no-op — provisioning must still succeed and the instance is usable.
info "asserting the instance provisioned cleanly despite no docker/buildx in the image"
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_out}" "alpha" "instance is missing after buildkit setup"

pass "buildkit enabled: the server is up and the instance can reach it"
