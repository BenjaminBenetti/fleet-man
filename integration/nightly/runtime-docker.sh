#!/usr/bin/env bash
# runtime-docker.sh — fleet on a node whose container runtime is docker
# (cri-dockerd) rather than containerd, which the integration-k8s shards'
# minikube runs: the lifecycle tests 010-061 and the cluster's own 800-807
# (interrupted create, tampered pod, crash loop, API cut, bundled tmux,
# compose refusal, exec polling, gateway delivery) on a throwaway minikube
# --container-runtime=docker.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

nightly_begin runtime-docker
nightly_require minikube kubectl docker devcontainer tmux
profile=fleet-docker-runtime
nightly_minikube "${profile}" --memory=6g --cpus=4 --container-runtime=docker
trap 'nightly_minikube_delete "${profile}"' EXIT
runtime=$(kubectl --context "${profile}" get node "${profile}" -o jsonpath='{.status.nodeInfo.containerRuntimeVersion}')
case "${runtime}" in docker://*) ;; *) nightly_say "the node runs ${runtime}, not docker"; exit 2 ;; esac
nightly_note "- node runtime ${runtime}"

nightly_itests "${profile}" 010-062 || nightly_dump "${profile}" lifecycle
NIGHTLY_ITEST_LABEL=cluster nightly_itests "${profile}" 800-808 || nightly_dump "${profile}" cluster
nightly_end
