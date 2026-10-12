#!/usr/bin/env bash
# scale.sh [instances] — fleetd's scale budgets with <instances> (default
# 100) lightweight pods on a throwaway one-node minikube, and the reconnect
# storm of a fleetd restart: tests/850_k8s_scale_budgets.sh. The node gets
# FLEET_NIGHTLY_SCALE_MEMORY (default 4g + 80m per instance) and every CPU.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

n="${1:-100}"
nightly_begin "scale-${n}"
nightly_require minikube kubectl docker devcontainer tmux
profile="fleet-scale-${n}"
memory="${FLEET_NIGHTLY_SCALE_MEMORY:-$(( 4096 + 80 * n ))m}"
nightly_minikube "${profile}" --memory="${memory}" --cpus="${FLEET_NIGHTLY_SCALE_CPUS:-$(nproc)}" \
  --extra-config=kubelet.max-pods="$(( n + 60 ))"
trap 'nightly_minikube_delete "${profile}"' EXIT

report="${NIGHTLY_OUT}/scale.tsv"
: > "${report}"
nightly_itests "${profile}" 850-851 FLEET_ITEST_SCALE="${n}" FLEET_ITEST_SCALE_REPORT="${report}" \
  FLEET_ITEST_SCALE_PARALLEL="${FLEET_NIGHTLY_SCALE_PARALLEL:-10}" || nightly_dump "${profile}" scale
nightly_budgets "${report}" "fleetd with ${n} pods" || true
nightly_end
