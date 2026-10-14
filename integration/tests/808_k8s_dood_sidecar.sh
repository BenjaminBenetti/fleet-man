#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, a docker-outside-of-docker devcontainer gets the pod's own docker engine (the fleet-docker sidecar): the remote user runs containers through the bound socket, siblings see the workspace and are pod-local (the create says so once), the engine's images survive a stop and start and a rebuild, and a change to its shared daemon.json reloads it. With Allow privileged pods off, the TUI's New instance dialog warns on the cluster's Deploy row that the fleet's DooD (then DinD) devcontainer needs them, and DooD and DinD creates fail before the build, naming the toggle, and leave nothing.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the docker engine's sidecar"

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-dood" setup_test

inst="${FIXTURE_REPO_NAME}/alpha"
in_instance() { "${FLEET_BIN}" exec "${inst}" -- "$@"; }

wait_docker() {  # <when>
  local deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
  until in_instance docker info >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "docker never answered in the instance (${1}): $(in_instance docker info 2>&1 | tail -n 3)"
    sleep 2
  done
}

info "fleet up alpha (docker-outside-of-docker)"
up_out=$(fleet_up alpha 2>&1) || fail "fleet up failed: ${up_out}"
printf '%s\n' "${up_out}" | tail -n 3
wait_docker "after create"

cid=$(itest_container_id "${inst}")
pod=$(itest_k8s_pod "${cid}")
containers=$(itest_k8s_kubectl get pod "${pod}" -o 'jsonpath={range .spec.initContainers[*]}{.name}={.restartPolicy} {end}')
assert_contains "${containers}" "fleet-docker=Always" "the engine should be the pod's native sidecar"

# The bound socket is a link to the sidecar's, and the remote user reaches
# it as on a Linux docker host: the feature joins the socket's group.
assert_equals "/opt/fleet/docker/docker.sock" "$(in_instance readlink /var/run/docker-host.sock)" "the bind target's link"
assert_equals "vscode" "$(in_instance id -un)" "the remote user"
run_out=$(in_instance docker run --rm hello-world 2>&1) || fail "docker run as the remote user failed: ${run_out}"
assert_contains "${run_out}" "Hello from Docker!" "the sidecar's engine should run containers"

# The engine is the pod's: its networks at the pod's MTU, its siblings
# pod-local, seeing the workspace at the dev container's path.
pod_mtu=$(in_instance cat /sys/class/net/eth0/mtu)
assert_equals "${pod_mtu}" "$(in_instance docker network inspect bridge --format '{{index .Options "com.docker.network.driver.mtu"}}')" \
  "the engine's bridge should take the pod network's MTU"
folder=$(in_instance sh -c 'pwd')
in_instance sh -c 'echo from-the-workspace > sibling-check'
assert_equals "from-the-workspace" "$(in_instance sh -c 'docker run --rm -v "$PWD":/w busybox:1.36 cat /w/sibling-check 2>/dev/null')" \
  "a sibling's bind of the workspace (${folder})"
warn_file="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-alpha.warn"
[ -f "${warn_file}" ] || fail "no warning banner for alpha"
assert_contains "$(cat "${warn_file}")" "pod-local siblings" "the create should say once that siblings are pod-local"
log="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-alpha.log"
assert_equals "1" "$(grep -c 'pod-local siblings' "${log}")" "the warning in the creation log, once"

# The shared daemon.json: the image cache step's file; a change reloads it.
cfg=$(in_instance printenv FLEET_DOCKERD_CONFIG)
assert_equals "/opt/fleet/docker/daemon.json" "${cfg}" "FLEET_DOCKERD_CONFIG"
# Root's, as /etc/docker on a docker host: its runtimes run as root in the
# privileged sidecar.
assert_equals "755:0" "$(in_instance stat -c %a:%u /opt/fleet/docker)" "the shared folder's mode and owner"
if in_instance sh -c "echo '{}' > ${cfg}.new && mv ${cfg}.new ${cfg}" >/dev/null 2>&1; then
  fail "the remote user replaced the engine's daemon.json"
fi
in_instance sh -c "echo '{\"registry-mirrors\":[\"http://itest-mirror.invalid:5000\"]}' | sudo tee ${cfg} >/dev/null"
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
until in_instance docker info --format '{{.RegistryConfig.Mirrors}}' 2>/dev/null | grep -q itest-mirror.invalid; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the engine did not reload its daemon.json"
  sleep 1
done
in_instance sh -c "echo '{}' | sudo tee ${cfg} >/dev/null"

in_instance docker tag hello-world itest/kept:v1
image_id=$(in_instance docker image inspect --format '{{.Id}}' itest/kept:v1)
info "fleet stop + start alpha"
"${FLEET_BIN}" stop "${inst}" >/dev/null
"${FLEET_BIN}" start "${inst}" >/dev/null
wait_docker "after a stop and start"
assert_equals "${image_id}" "$(in_instance docker image inspect --format '{{.Id}}' itest/kept:v1 2>&1)" \
  "the engine's image should survive a stop and start (it lives on vol/fleet-docker)"

info "fleet rebuild alpha: a new root, the engine's volume kept"
before_id=$(itest_container_id "${inst}")
"${FLEET_BIN}" rebuild "${inst}" >/dev/null
[ "$(itest_container_id "${inst}")" != "${before_id}" ] || fail "the rebuild kept the container id ${before_id}"
wait_docker "after a rebuild"
assert_equals "${image_id}" "$(in_instance docker image inspect --format '{{.Id}}' itest/kept:v1 2>&1)" \
  "the engine's image should survive a rebuild (vol/fleet-docker is not part of the root)"

# deploy_row_warns <text> — the TUI's New instance dialog, opened on the
# fleet, shows <text> under the cluster's Deploy row (the inspector's
# check/kubernetes on the fleet's newest checkout).
deploy_row_warns() {
  local dialog
  tui_spawn
  tui_wait_for "alpha" 15 || fail "the TUI did not show the fleet"
  tui_send a
  tui_wait_for "instance-name" 5 || fail "the New instance dialog did not open"
  sleep 1
  # The dialog box wraps the warning; read it with the box's borders out.
  dialog=$(tui_capture | perl -CS -Mutf8 -pe 's/[│╭╮╰╯─]//g' | tr -s ' \n' '  ')
  assert_contains "${dialog}" "$1" "the Deploy row's warning"
  tui_send Escape
  sleep 0.3
  tui_kill
}

# refused_before_build <name> <code> <warning> — a create of <name> fails with
# <code>, naming the toggle, before the build, and leaves no object; with its
# checkout the newest, the Deploy row warns with <warning>.
refused_before_build() {
  local name="$1" code="$2" warning="$3" rc=0 out key
  info "fleet up ${name} (Allow privileged pods off)"
  out=$("${FLEET_BIN}" up "${name}" --repo "${FIXTURE_REPO_URL}" 2>&1) || rc=$?
  printf '%s\n' "${out}" | tail -n 3
  [ "${rc}" -ne 0 ] || fail "${name} was created without privileged pods"
  assert_contains "${out}" "${code}" "${name}'s error code"
  assert_contains "${out}" "Allow privileged pods" "${name}'s error should name the toggle"
  assert_not_contains "$(cat "${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-${name}.log")" "==> building image" \
    "${name} should be refused before the build"
  key=$(itest_k8s_key "${FIXTURE_REPO_NAME}/${name}")
  [ -n "${key}" ] || fail "the refused record ${name} has no key"
  assert_equals "" "$(itest_k8s_key_objects "${key}")" "nothing is applied for a refused ${name}"
  deploy_row_warns "${warning}"
  "${FLEET_BIN}" down "${FIXTURE_REPO_NAME}/${name}" >/dev/null
}

"${FLEET_BIN}" cluster set itest allow_privileged=false >/dev/null
refused_before_build beta kubernetes:unsupported:dood-needs-privileged \
  "gets a docker engine sidecar there, which needs privileged pods (Allow privileged pods, Settings → Kubernetes)"
cp "${INTEGRATION_DIR}/fixture-dind/.devcontainer/devcontainer.json" "${FIXTURE_REPO_DIR}/.devcontainer/devcontainer.json"
(cd "${FIXTURE_REPO_DIR}" && git commit -qam "fixture: docker-in-docker")
refused_before_build gamma kubernetes:unsupported:privileged \
  "needs privileged pods, which are not allowed there (Allow privileged pods, Settings → Kubernetes)"

info "fleet down alpha"
alpha_key=$(itest_k8s_key "${inst}")
"${FLEET_BIN}" down "${inst}" >/dev/null
itest_k8s_wait_no_objects "${alpha_key}" 120

pass "docker-outside-of-docker: the pod's own engine as a sidecar, refused before the build without privileged pods"
