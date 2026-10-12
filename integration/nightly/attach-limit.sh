#!/usr/bin/env bash
# attach-limit.sh — a node that takes few volumes: a throwaway minikube with
# the csi-hostpath driver limited to FLEET_NIGHTLY_ATTACH_LIMIT (3) volumes
# per node (clusters/csi-hostpath-limit.sh), then 825 (the fleet-size cap,
# the instance past it Pending with the limit named) and 826 (a share moved
# between classes).
set -euo pipefail
source "$(dirname "$0")/lib.sh"

nightly_begin attach-limit
nightly_require minikube kubectl docker devcontainer tmux
profile=fleet-attach-limit
nightly_minikube "${profile}" --memory=6g --cpus=4 --addons=volumesnapshots,csi-hostpath-driver
trap 'nightly_minikube_delete "${profile}"' EXIT
"${REPO_ROOT}/integration/clusters/csi-hostpath-limit.sh" "${profile}" "${FLEET_NIGHTLY_ATTACH_LIMIT:-3}" >/dev/null

nightly_itests "${profile}" 825-827 || nightly_dump "${profile}" attach-limit
nightly_end
