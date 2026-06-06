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

if [ "${FLEET_QA_START_DOCKER:-1}" = "1" ]; then
  start_dockerd || log "continuing without a working docker daemon"
fi

if [ "$#" -eq 0 ]; then
  exec bash -l
fi
exec "$@"
