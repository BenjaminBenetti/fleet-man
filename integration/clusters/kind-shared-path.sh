#!/usr/bin/env bash
# kind-shared-path.sh — a throwaway multi-node kind cluster for the
# Kubernetes itests a single minikube node cannot run: placement by a fleet's
# share (818) and a fleet moved off a drained node (819).
#
#   kind-shared-path.sh up   [name] [workers]   # default p405 1: two nodes
#   kind-shared-path.sh down [name]
#
# `up` makes kind-<name> (the kube context it prints) with a control plane
# that takes pods and <workers> workers, then a StorageClass,
# fleet-shared-path (WaitForFirstConsumer), whose volumes are folders of one
# host directory mounted on every node: data that outlives its node, as a
# network disk's does, so a fleet can move between nodes. kind's own default
# class (standard, local-path) stays: its volumes are each on one node's disk.
# The class is provisioned by a second copy of kind's local-path provisioner
# in shared-filesystem mode, from the images kind already loaded.
#
# Run it where the itests run (the QA container), then:
#
#   FLEET_ITEST_BACKEND=kubernetes FLEET_ITEST_CLUSTER=kind-<name> \
#     FLEET_ITEST_RANGE=818-820 ./integration/run.sh
#
# Each node container is capped at FLEET_KIND_NODE_MEMORY (4g) and
# FLEET_KIND_NODE_CPUS (2). The shared directory is FLEET_KIND_SHARED_DIR
# (default ~/.fleet-kind/<name>); `down` deletes the cluster and it.
set -euo pipefail

cmd="${1:-}"
name="${2:-p405}"
workers="${3:-1}"
shared="${FLEET_KIND_SHARED_DIR:-${HOME}/.fleet-kind/${name}}"
ctx="kind-${name}"
k() { kubectl --context "${ctx}" "$@"; }

up() {
  mkdir -p "${shared}"
  local cfg
  cfg=$(mktemp)
  {
    printf 'kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n'
    local role
    for role in control-plane $(for _ in $(seq "${workers}"); do printf 'worker '; done); do
      printf -- '- role: %s\n  extraMounts:\n  - {hostPath: %s, containerPath: /var/lib/fleet-shared}\n' "${role}" "${shared}"
    done
  } > "${cfg}"
  kind create cluster --name "${name}" --config "${cfg}" --wait 5m
  rm -f "${cfg}"
  local node
  for node in $(kind get nodes --name "${name}"); do
    docker update --memory "${FLEET_KIND_NODE_MEMORY:-4g}" --memory-swap "${FLEET_KIND_NODE_MEMORY:-4g}" \
      --cpus "${FLEET_KIND_NODE_CPUS:-2}" "${node}" >/dev/null
  done
  # Every node takes pods: a fleet has somewhere to go.
  k taint nodes -l node-role.kubernetes.io/control-plane node-role.kubernetes.io/control-plane:NoSchedule- >/dev/null 2>&1 || true
  k wait --for=condition=Ready nodes --all --timeout=180s >/dev/null

  local image helper
  image=$(k -n local-path-storage get deployment local-path-provisioner -o 'jsonpath={.spec.template.spec.containers[0].image}')
  helper=$(k -n local-path-storage get configmap local-path-config -o 'jsonpath={.data.helperPod\.yaml}' | awk '/image:/ {print $2; exit}')
  [ -n "${image}" ] && [ -n "${helper}" ] || { echo "kind-shared-path: kind's local-path provisioner not found" >&2; exit 1; }
  k apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata: {name: fleet-shared-path-config, namespace: local-path-storage}
data:
  config.json: |-
    {"sharedFileSystemPath": "/var/lib/fleet-shared"}
  helperPod.yaml: |-
    apiVersion: v1
    kind: Pod
    metadata:
      name: helper-pod
    spec:
      priorityClassName: system-node-critical
      tolerations:
        - {operator: Exists, effect: NoSchedule}
        - {operator: Exists, effect: NoExecute}
      containers:
      - name: helper-pod
        image: ${helper}
        imagePullPolicy: IfNotPresent
  setup: |-
    #!/bin/sh
    set -eu
    mkdir -m 0777 -p "\$VOL_DIR"
  teardown: |-
    #!/bin/sh
    set -eu
    rm -rf "\$VOL_DIR"
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: fleet-shared-path-provisioner, namespace: local-path-storage}
spec:
  replicas: 1
  selector: {matchLabels: {app: fleet-shared-path-provisioner}}
  template:
    metadata: {labels: {app: fleet-shared-path-provisioner}}
    spec:
      serviceAccountName: local-path-provisioner-service-account
      containers:
      - name: provisioner
        image: ${image}
        imagePullPolicy: IfNotPresent
        command: [local-path-provisioner, start, --provisioner-name, fleet.dev/shared-path, --configmap-name, fleet-shared-path-config, --helper-image, "${helper}", --config, /etc/config/config.json]
        env:
        - name: POD_NAMESPACE
          valueFrom: {fieldRef: {fieldPath: metadata.namespace}}
        volumeMounts:
        - {name: config, mountPath: /etc/config}
      volumes:
      - name: config
        configMap: {name: fleet-shared-path-config}
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: fleet-shared-path}
provisioner: fleet.dev/shared-path
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Delete
EOF
  k -n local-path-storage rollout status deployment/fleet-shared-path-provisioner --timeout=180s >/dev/null
  echo "${ctx}"
}

down() {
  kind delete cluster --name "${name}" >/dev/null 2>&1 || true
  # The pods made root-owned files there.
  [ ! -d "${shared}" ] || sudo -n rm -rf "${shared}" 2>/dev/null || rm -rf "${shared}" 2>/dev/null || true
}

case "${cmd}" in
  up) up ;;
  down) down ;;
  *) echo "usage: $0 up|down [name] [workers]" >&2; exit 2 ;;
esac
