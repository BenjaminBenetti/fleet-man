#!/bin/bash
# Turn the dev minikube cluster on or off. It is off by default to save memory.
#
#   .devcontainer/minikube.sh on [minikube start flags]   e.g. --memory=8g --cpus=4
#   .devcontainer/minikube.sh off
#   .devcontainer/minikube.sh status
#
# `on` leaves `minikube` as the current kubectl context, so kubectl, kubectx,
# kubens and k9s target it with no further setup. `off` stops the cluster (and
# drops its kubectl context) but keeps its state for a quick restart;
# `minikube delete` wipes it.

set -euo pipefail

export MINIKUBE_WANTUPDATENOTIFICATION=false

ensure_docker() {
  docker info >/dev/null 2>&1 && return
  if [ -x /usr/local/share/docker-init.sh ]; then
    sudo /usr/local/share/docker-init.sh /bin/true >/dev/null 2>&1 || true
  fi
  for _ in $(seq 30); do
    docker info >/dev/null 2>&1 && return
    sleep 1
  done
  echo "minikube.sh: docker daemon is not reachable" >&2
  exit 1
}

case "${1:-}" in
  on)
    shift
    if [ $# -eq 0 ] && minikube status >/dev/null 2>&1; then
      echo "minikube is already running"
    else
      ensure_docker
      minikube start --driver=docker "$@"
    fi
    kubectl config use-context minikube >/dev/null
    # start returns before the CNI is up; wait so `on` means pods can schedule.
    kubectl wait --for=condition=Ready node --all --timeout=180s >/dev/null
    ;;
  off)
    minikube stop
    ;;
  status)
    minikube status
    ;;
  *)
    echo "usage: $0 on [minikube start flags] | off | status" >&2
    exit 2
    ;;
esac
