#!/usr/bin/env bash
# expand.sh — volumes that fill and grow: a throwaway minikube with a
# StorageClass whose volumes hold only their size and can grow
# (clusters/lvm-localpv.sh: OpenEBS LVM LocalPV on a loop-file volume group
# in the node), then 854 (a share that fills, pauses the sync and grows by
# half with auto-expand on; a cache volume growing the same way).
set -euo pipefail
source "$(dirname "$0")/lib.sh"

nightly_begin expand
nightly_require minikube kubectl docker devcontainer tmux curl sha256sum
profile=fleet-expand
nightly_minikube "${profile}" --memory=6g --cpus=4
trap '"${REPO_ROOT}/integration/clusters/lvm-localpv.sh" down "${profile}" || true; nightly_minikube_delete "${profile}"' EXIT
class=$("${REPO_ROOT}/integration/clusters/lvm-localpv.sh" up "${profile}" 20G)
kubectl --context "${profile}" get storageclass >&2

nightly_itests "${profile}" 854-855 FLEET_ITEST_EXPAND_CLASS="${class}" || nightly_dump "${profile}" expand
nightly_end
