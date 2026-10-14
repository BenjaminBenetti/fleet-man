#!/usr/bin/env bash
# itest: k8s
# Description: a docker-in-docker instance runs its own dockerd that pulls and runs containers, and its images survive stop/start and rebuild (the feature keeps /var/lib/docker on a per-instance volume) until the instance is removed. On a cluster the nested engine runs at the pod network's MTU and nests its containers in the pod's own cgroups.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-dind" setup_test

inst="${FIXTURE_REPO_NAME}/alpha"
in_instance() { "${FLEET_BIN}" exec "${inst}" -- "$@"; }

# wait_inner_dockerd <when> — the feature's entrypoint starts dockerd in the
# background on every container start, so give it a moment to answer.
wait_inner_dockerd() {
  local deadline=$(( $(date +%s) + $(_scale_timeout 90) ))
  until in_instance docker info >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "the instance's dockerd never answered (${1}): $(in_instance docker info 2>&1 | tail -n 3)"
    sleep 2
  done
}

info "fleet up alpha (docker-in-docker)"
up_out=$(fleet_up alpha 2>&1) || fail "fleet up failed: ${up_out}"
printf '%s\n' "${up_out}" | tail -n 3
wait_inner_dockerd "after create"

# The instance's docker CLI talks to the instance's own daemon, not the host's.
in_instance sh -c 'test -S /var/run/docker.sock' || fail "no docker socket inside the instance"
inner_root=$(in_instance docker info --format '{{.DockerRootDir}}')
assert_equals "/var/lib/docker" "${inner_root}" "the inner dockerd should keep its data in /var/lib/docker"
in_instance sh -c 'grep -q " /var/lib/docker " /proc/self/mountinfo' \
  || fail "/var/lib/docker should be its own mount (the feature's volume)"

info "pulling and running a container inside the instance"
run_out=$(in_instance docker run --rm hello-world 2>&1) || fail "docker run inside the instance failed: ${run_out}"
assert_contains "${run_out}" "Hello from Docker!" "the inner dockerd should run containers"

# check_k8s_engine <when> — on a cluster, pod-init configured the image's
# dockerd before it started: its networks at the pod network's MTU (a nested
# network above it fragments under an overlay CNI), and its containers'
# cgroups below fleet-user, inside the pod (a privileged container shares
# the node's cgroup namespace, where dockerd's default /docker parent is the
# node's root), with their controllers, so a memory limit works.
check_k8s_engine() {
  itest_backend_is kubernetes || return 0
  local pod_mtu bridge_mtu dockerd_cg inner
  pod_mtu=$(in_instance cat /sys/class/net/eth0/mtu)
  bridge_mtu=$(in_instance docker network inspect bridge --format '{{index .Options "com.docker.network.driver.mtu"}}')
  info "${1}: pod MTU ${pod_mtu}, the engine's bridge ${bridge_mtu}"
  assert_equals "${pod_mtu}" "${bridge_mtu}" "the nested engine's bridge should take the pod network's MTU (${1})"
  dockerd_cg=$(in_instance sh -c 'for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = dockerd ] && sed -n "s/^0:://p" $p/cgroup && break; done')
  case "${dockerd_cg}" in
    */fleet-user/procs) ;;
    *) fail "dockerd should run in fleet-user/procs, runs in '${dockerd_cg}' (${1})" ;;
  esac
  inner=$(in_instance docker run --rm -m 64m busybox:1.36 sh -c 'cat /sys/fs/cgroup/memory.max; cat /proc/self/cgroup' 2>&1) \
    || fail "a container with a memory limit failed (${1}): ${inner}"
  assert_contains "${inner}" "67108864" "the limited container's memory.max (${1})"
  assert_contains "$(in_instance sudo cat /etc/docker/daemon.json)" "${dockerd_cg%/procs}/docker" \
    "the engine's cgroup parent should be fleet-user/docker (${1})"
}
check_k8s_engine "after create"
# An image and a container of its own, both kept in the inner /var/lib/docker.
in_instance docker tag hello-world itest/kept:v1
in_instance docker create --name itest-inner itest/kept:v1 >/dev/null
image_id=$(in_instance docker image inspect --format '{{.Id}}' itest/kept:v1)
[ -n "${image_id}" ] || fail "could not read the inner image id"

info "fleet stop + start alpha"
"${FLEET_BIN}" stop "${inst}" >/dev/null
"${FLEET_BIN}" start "${inst}" >/dev/null
wait_inner_dockerd "after a stop and start"
assert_equals "${image_id}" "$(in_instance docker image inspect --format '{{.Id}}' itest/kept:v1 2>&1)" \
  "the inner image should survive a stop and start"
assert_contains "$(in_instance docker ps -a --format '{{.Names}}')" "itest-inner" \
  "the inner container should survive a stop and start"
run_out=$(in_instance docker run --rm itest/kept:v1 2>&1) || fail "docker run after a restart failed: ${run_out}"
assert_contains "${run_out}" "Hello from Docker!" "the inner dockerd should still run containers after a restart"
check_k8s_engine "after a stop and start"

if itest_backend_rebuilds; then
  info "fleet rebuild alpha"
  old_container=$(itest_container_id "${inst}")
  "${FLEET_BIN}" rebuild "${inst}" >/dev/null
  [ "$(itest_container_id "${inst}")" != "${old_container}" ] || fail "rebuild should replace the container"
  wait_inner_dockerd "after a rebuild"
  assert_equals "${image_id}" "$(in_instance docker image inspect --format '{{.Id}}' itest/kept:v1 2>&1)" \
    "the inner image should survive a rebuild (it lives on the instance's docker volume)"
fi

if itest_backend_is devcontainer; then
  # The volume is a docker named volume on the daemon host; removing the
  # instance must remove it too, or every destroyed DinD instance leaks its
  # images. Clusters keep it on the instance's own disk instead.
  volumes=$(docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}' "$(itest_container_id "${inst}")")
  info "instance volumes: ${volumes}"
  [ -n "${volumes}" ] || fail "the DinD instance should have a docker volume for /var/lib/docker"
fi

info "fleet down alpha"
"${FLEET_BIN}" down "${inst}" >/dev/null
[ -z "$(itest_container_leftovers "${inst}")" ] || fail "containers left for alpha after down"
if itest_backend_is devcontainer; then
  for v in ${volumes}; do
    if docker volume inspect "${v}" >/dev/null 2>&1; then
      fail "down left the DinD volume ${v} behind"
    fi
  done
fi

if itest_backend_rebuilds; then
  pass "docker-in-docker: inner dockerd works and its state survives stop/start and rebuild"
fi
pass "docker-in-docker: inner dockerd works and its state survives stop/start (this backend does not rebuild yet)"
