#!/usr/bin/env bash
# kind-nfs.sh — fleet's shares on a ReadWriteMany class across nodes: a
# throwaway two-node kind cluster (clusters/kind-shared-path.sh: kind's
# node-local default class plus fleet-shared-path) with csi-driver-nfs and a
# kernel NFS server on a tmpfs in the cluster, then the share itests 817-827
# (823: Auto picks the NFS class after the multi-node test, siblings on two
# nodes; 826 moves a share to it) and 826 again onto fleet-shared-path in
# multi-node mode.
#
# The NFS teardown has an order: a hard NFS mount whose server goes first
# hangs its node in the host kernel (NFSv4 DELEGRETURN), so the consumers
# go first, then the driver, then the server, each waited for.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

nightly_begin kind-nfs
nightly_require kind kubectl docker devcontainer tmux curl sha256sum
name=fleet-nfs
driver_version=v4.11.0
# The driver's manifests at that release, checked.
declare -A manifests=(
  [rbac-csi-nfs.yaml]=312de689d07627330d8280e50d7b51357e0b5f5ca0d8aa75794aec460ee4cff0
  [csi-nfs-driverinfo.yaml]=fa2bba4674821b3317d1e016f3c9d54abe04fcdc6fed3fce9e58994c8311c4a0
  [csi-nfs-controller.yaml]=0cd516bb18de3d1900f2fb5798107f633bd05a39c6f3954b88640a16a744cab9
  [csi-nfs-node.yaml]=5771ee567b80db6aa7c94a3f83f09115e5e1a4786b907a3a7f0052a393b14a02
)
# Kubernetes e2e's NFS server (kernel nfsd, /exports as the NFSv4 root),
# from registry.k8s.io: Docker Hub rate-limits anonymous pulls.
nfs_image=registry.k8s.io/e2e-test-images/volume/nfs:1.4@sha256:c3e380bb5a92095288b4dd586a250c560612afbabf53fb2a057e99fff75f038f
manifest_dir="${NIGHTLY_OUT}/csi-driver-nfs"
mkdir -p "${manifest_dir}"
for f in "${!manifests[@]}"; do
  curl -fsSL -o "${manifest_dir}/${f}" "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/${driver_version}/deploy/${driver_version}/${f}"
  echo "${manifests[${f}]}  ${manifest_dir}/${f}" | sha256sum -c --quiet - || { nightly_say "${f} of csi-driver-nfs ${driver_version} is not the one checked"; exit 2; }
done

ctx=$("${REPO_ROOT}/integration/clusters/kind-shared-path.sh" up "${name}" 1)
k() { kubectl --context "${ctx}" "$@"; }
nfs_mounts() {
  local node
  for node in $(kind get nodes --name "${name}" 2>/dev/null); do
    docker exec "${node}" grep -cE ' nfs4? ' /proc/mounts 2>/dev/null || true
  done | awk '{ s += $1 } END { print s + 0 }'
}
teardown() {
  [ -n "${FLEET_NIGHTLY_KEEP:-}" ] && { nightly_say "keeping kind ${name}"; return 0; }
  nightly_say "teardown: the NFS consumers first"
  pkill -f "[f]leet server" 2>/dev/null || true
  local ns pods=0 pvs=0 mounts=0
  for ns in $(k get ns -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    case "${ns}" in kube-system | kube-public | kube-node-lease | default | nfs-server | local-path-storage) ;; *) k delete ns "${ns}" --wait=false >/dev/null 2>&1 || true ;; esac
  done
  k delete pvc --all -A --wait=false >/dev/null 2>&1 || true
  for _ in $(seq 1 90); do
    pods=$(k get pods -A --no-headers 2>/dev/null | grep -cvE '^(kube-system|nfs-server|local-path-storage) ' || true)
    pvs=$(k get pv --no-headers 2>/dev/null | grep -c nfs-csi || true)
    mounts=$(nfs_mounts)
    [ "${pods}" = 0 ] && [ "${pvs}" = 0 ] && [ "${mounts}" = 0 ] && break
    sleep 2
  done
  nightly_say "teardown: ${pods} pods, ${pvs} NFS volumes, ${mounts} NFS mounts left; now the driver"
  for f in csi-nfs-node.yaml csi-nfs-controller.yaml; do k delete -f "${manifest_dir}/${f}" --wait=true --timeout=120s >/dev/null 2>&1 || true; done
  for _ in $(seq 1 30); do [ "$(nfs_mounts)" = 0 ] && break; sleep 2; done
  nightly_say "teardown: $(nfs_mounts) NFS mounts left; now the server, then the cluster"
  k -n nfs-server delete deployment nfs-server --wait=true --timeout=120s >/dev/null 2>&1 || true
  "${REPO_ROOT}/integration/clusters/kind-shared-path.sh" down "${name}"
}
trap teardown EXIT

k create namespace nfs-server >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata: {name: nfs-server, namespace: nfs-server, labels: {app: nfs-server}}
spec:
  selector: {app: nfs-server}
  ports:
  - {name: tcp-2049, port: 2049, protocol: TCP}
  - {name: tcp-20048, port: 20048, protocol: TCP}
  - {name: udp-111, port: 111, protocol: UDP}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: nfs-server, namespace: nfs-server}
spec:
  replicas: 1
  selector: {matchLabels: {app: nfs-server}}
  template:
    metadata: {labels: {app: nfs-server}}
    spec:
      containers:
      - name: nfs-server
        image: ${nfs_image}
        securityContext: {privileged: true}
        ports:
        - {name: tcp-2049, containerPort: 2049, protocol: TCP}
        - {name: tcp-20048, containerPort: 20048, protocol: TCP}
        - {name: udp-111, containerPort: 111, protocol: UDP}
        volumeMounts: [{name: exports, mountPath: /exports}]
      # A tmpfs: the kernel's nfsd cannot export an overlay folder.
      volumes: [{name: exports, emptyDir: {medium: Memory, sizeLimit: 2Gi}}]
EOF
for f in rbac-csi-nfs.yaml csi-nfs-driverinfo.yaml csi-nfs-controller.yaml csi-nfs-node.yaml; do k apply -f "${manifest_dir}/${f}" >/dev/null; done
k apply -f - >/dev/null <<'EOF'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: nfs-csi}
provisioner: nfs.csi.k8s.io
parameters: {server: nfs-server.nfs-server.svc.cluster.local, share: /}
reclaimPolicy: Delete
volumeBindingMode: Immediate
allowVolumeExpansion: true
mountOptions: [nfsvers=4.1]
EOF
k -n nfs-server rollout status deployment/nfs-server --timeout=300s
k -n kube-system rollout status deployment/csi-nfs-controller --timeout=300s
k -n kube-system rollout status daemonset/csi-nfs-node --timeout=300s
k get nodes -o wide >&2
k get storageclass >&2
# NFSv4's grace period after the server starts holds locks for 90s.
sleep 95

nightly_itests "${ctx}" 817-828 FLEET_ITEST_SHARE_TO_CLASS=nfs-csi || nightly_dump "${ctx}" nfs
NIGHTLY_ITEST_LABEL=shared-path nightly_itests "${ctx}" 826-827 FLEET_ITEST_SHARE_TO_CLASS=fleet-shared-path \
  FLEET_ITEST_SHARE_MOVE_MODE=multi-node || nightly_dump "${ctx}" shared-path
nightly_end
