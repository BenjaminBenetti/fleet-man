#!/usr/bin/env bash
# Entry point for the fleet-man QA environment container.
#
# Brings up an in-container Docker daemon (Docker-in-Docker) so the fleet
# integration suite — which drives real devcontainers against a real daemon —
# has something to talk to, then hands off to the requested command (default:
# an interactive login shell).
#
# The container must be run with --privileged for the bundled daemon to start.
# If a usable daemon is already reachable (e.g. a host /var/run/docker.sock is
# mounted in), startup is skipped. Set FLEET_QA_START_DOCKER=0 to never start
# the bundled daemon.
#
# Set FLEET_QA_START_MINIKUBE=1 to also start a minikube cluster on that daemon
# (docker driver) and wait for its node to be Ready; FLEET_QA_MINIKUBE_FLAGS
# passes extra flags to `minikube start` (e.g. "--memory=8g --cpus=4").
set -euo pipefail

log() { printf '[qa-entrypoint] %s\n' "$*" >&2; }

# --- iptables backend fix -------------------------------------------------
# dockerd's bridge networking needs the iptables backend to match the host
# kernel. Modern kernels use nf_tables; if the container still points iptables
# at the legacy backend, dockerd fails to set up its network. Mirror the
# devcontainer's detection (.devcontainer/setup.sh) and switch to nft if needed.
fix_iptables_backend() {
  command -v update-alternatives >/dev/null 2>&1 || return 0
  [ -x /usr/sbin/iptables-nft ] || return 0

  local current
  current="$(update-alternatives --query iptables 2>/dev/null | awk '/^Value:/{print $2}')"
  [ "${current}" = "/usr/sbin/iptables-nft" ] && return 0

  if [ -d /sys/module/nf_tables ] || grep -q nf_tables /proc/modules 2>/dev/null \
     || ! /usr/sbin/iptables-legacy -L -n >/dev/null 2>&1; then
    log "switching iptables backend to nft"
    sudo update-alternatives --set iptables /usr/sbin/iptables-nft >/dev/null 2>&1 || true
    if [ -x /usr/sbin/ip6tables-nft ]; then
      sudo update-alternatives --set ip6tables /usr/sbin/ip6tables-nft >/dev/null 2>&1 || true
    fi
  fi
}

# --- cgroup v2 nesting ----------------------------------------------------
# Under cgroup v2 the container starts at the root of its own cgroup namespace
# with no controllers delegated to children. Nested containers then can't take
# resource limits (minikube's `docker run --memory` fails with "cannot enter
# cgroupv2 ... in threaded mode"), and k3s's kubelet has none to manage. A
# cgroup with processes in it can't delegate controllers, so move every process
# into an /init child first, then enable them all for children. This is the
# moby dind wrapper's fix, which the devcontainer's docker-in-docker feature
# also applies. It runs only at the root of a private cgroup namespace (under
# --cgroupns=host /sys/fs/cgroup is the host's hierarchy; on a second run
# everything already lives in /init) and only when cgroupfs is writable (it is
# read-only without --privileged).
enable_cgroup_nesting() {
  [ -f /sys/fs/cgroup/cgroup.controllers ] || return 0
  [ "$(cat /proc/self/cgroup 2>/dev/null)" = "0::/" ] || return 0
  awk '$2 == "/sys/fs/cgroup" { split($4, o, ","); rw = (o[1] == "rw") } END { exit !rw }' \
    /proc/self/mounts || return 0

  # An error moving a process that already exited (the xargs) is ignored.
  if ! sudo sh -c '
    mkdir -p /sys/fs/cgroup/init
    xargs -rn1 </sys/fs/cgroup/cgroup.procs >/sys/fs/cgroup/init/cgroup.procs 2>/dev/null || :
    sed -e "s/ / +/g" -e "s/^/+/" </sys/fs/cgroup/cgroup.controllers >/sys/fs/cgroup/cgroup.subtree_control
  '; then
    log "could not enable cgroup v2 nesting; nested clusters may not start"
  fi
}

start_dockerd() {
  if docker info >/dev/null 2>&1; then
    log "docker daemon already reachable; not starting the bundled one"
    return 0
  fi

  fix_iptables_backend
  log "starting dockerd (Docker-in-Docker)…"
  # The redirect runs under sudo so root owns the log file, and dockerd is
  # backgrounded inside that root shell so it survives this script.
  sudo rm -f /var/run/docker.pid
  sudo sh -c 'nohup dockerd >/var/log/dockerd.log 2>&1 &'

  local timeout="${FLEET_QA_DOCKERD_TIMEOUT:-60}"
  for ((i = 0; i < timeout; i++)); do
    if docker info >/dev/null 2>&1; then
      log "docker daemon is up"
      return 0
    fi
    sleep 1
  done

  log "dockerd did not become ready within ${timeout}s — last log lines:"
  sudo tail -n 40 /var/log/dockerd.log >&2 || true
  return 1
}

# Mirrors .devcontainer/minikube.sh on: minikube becomes the current kubectl
# context, and startup waits for the node to be Ready so pods can schedule.
start_minikube() {
  if ! docker info >/dev/null 2>&1; then
    log "no docker daemon reachable; cannot start minikube"
    return 1
  fi

  local log_file=/tmp/qa-minikube.log
  if minikube status >/dev/null 2>&1; then
    log "minikube already running"
  elif [ "$(minikube profile list -o json 2>/dev/null \
             | jq 'any(.valid[], .invalid[]; .Name == "minikube")' 2>/dev/null)" != true ] \
       && docker container inspect minikube >/dev/null 2>&1; then
    # A shared daemon (a mounted host socket) can hold another minikube's node,
    # such as the devcontainer's. Without its profile here, `minikube start`
    # takes that container for an abandoned one and deletes it.
    log "the daemon already has a minikube container this one didn't create; leaving it alone"
    return 1
  else
    log "starting minikube (docker driver)…"
    # shellcheck disable=SC2086 # FLEET_QA_MINIKUBE_FLAGS is a list of flags
    if ! minikube start --driver=docker ${FLEET_QA_MINIKUBE_FLAGS:-} >"${log_file}" 2>&1; then
      log "minikube start failed — last log lines (full log: ${log_file}):"
      tail -n 40 "${log_file}" >&2 || true
      return 1
    fi
  fi

  kubectl config use-context minikube >/dev/null
  local timeout="${FLEET_QA_MINIKUBE_TIMEOUT:-180}"
  if ! kubectl wait --for=condition=Ready node --all --timeout="${timeout}s" >/dev/null; then
    log "minikube node not Ready within ${timeout}s"
    return 1
  fi
  log "minikube is up ($(kubectl version 2>/dev/null | awk '/^Server Version:/{print $3}'))"
}

enable_cgroup_nesting

if [ "${FLEET_QA_START_DOCKER:-1}" = "1" ]; then
  start_dockerd || log "continuing without a working docker daemon"
fi

if [ "${FLEET_QA_START_MINIKUBE:-0}" = "1" ]; then
  start_minikube || log "continuing without a minikube cluster"
fi

if [ "$#" -eq 0 ]; then
  exec bash -l
fi
exec "$@"
