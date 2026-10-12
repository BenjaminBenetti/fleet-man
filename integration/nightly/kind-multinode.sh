#!/usr/bin/env bash
# kind-multinode.sh — placement and drains on several nodes: a throwaway
# three-node kind cluster (clusters/kind-shared-path.sh: every node takes
# pods; kind's node-local local-path default class, so fleets run in
# single-node mode, plus fleet-shared-path, whose volumes every node mounts)
# and the itests one minikube node cannot run: 817 (a fleet's share), 818
# (co-location while every instance is stopped, the Immediate-class
# refusal), 819 (a drain of the share's node with a TUI watching: the fleet
# moves), 820-822 (rebuilds), 852 (the deep test's two-pod probe and the
# one-node warning).
set -euo pipefail
source "$(dirname "$0")/lib.sh"

nightly_begin kind-multinode
nightly_require kind kubectl docker devcontainer tmux
name=fleet-multinode
ctx=$("${REPO_ROOT}/integration/clusters/kind-shared-path.sh" up "${name}" 2)
trap '[ -n "${FLEET_NIGHTLY_KEEP:-}" ] || "${REPO_ROOT}/integration/clusters/kind-shared-path.sh" down "${name}"' EXIT
kubectl --context "${ctx}" get nodes -o wide >&2
kubectl --context "${ctx}" get storageclass >&2

nightly_itests "${ctx}" 817-823 || nightly_dump "${ctx}" placement
NIGHTLY_ITEST_LABEL=deep nightly_itests "${ctx}" 852-853 || nightly_dump "${ctx}" deep
nightly_end
