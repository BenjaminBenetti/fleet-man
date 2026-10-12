#!/usr/bin/env bash
# lvm-localpv.sh — a StorageClass whose volumes hold only their size and can
# grow, on a throwaway minikube (docker driver): OpenEBS LVM LocalPV on a
# volume group of a sparse loop file inside the node, for the itests of
# full and growing volumes (854: FLEET_ITEST_EXPAND_CLASS=fleet-lvm).
# hostpath classes never fill (a folder on the node's disk) and do not
# grow.
#
#   lvm-localpv.sh up   <profile> [size]   # default size 20G
#   lvm-localpv.sh down <profile>
#
# The loop device is the host kernel's: `down` (after the PVCs on the class
# are gone) detaches it; a cluster deleted without it leaves the device
# holding the deleted file. Never run it on a shared cluster.
set -euo pipefail

cmd="${1:-}"
profile="${2:?usage: $0 up|down <profile> [size]}"
size="${3:-20G}"
version=v1.10.1
image=docker.io/openebs/lvm-driver:1.10.1@sha256:41f73aba7f31eee4a033053d9c2f203e007a838c14823f3fd472c50651a98999
manifest_sha256=9665fc6841e6a4439a66bff0032c48cbb64c7bf468a1917faccbe648e4f10ee4
file=/var/lib/fleet-lvm.img
k() { kubectl --context "${profile}" "$@"; }
node() { docker exec "${profile}" sh -c "$1"; }
plugin() {
  local pod
  pod=$(k -n kube-system get pod -l app=openebs-lvm-node -o jsonpath='{.items[0].metadata.name}')
  k -n kube-system exec "${pod}" -c openebs-lvm-plugin -- sh -c "$1"
}

up() {
  # The loop device: a free one, its node made in the node container's
  # /dev (a tmpfs that has only the devices of its start).
  node "[ -e ${file} ] || truncate -s ${size} ${file}
    d=\$(losetup -j ${file} | cut -d: -f1)
    if [ -z \"\$d\" ]; then
      d=\$(losetup -f); [ -e \"\$d\" ] || mknod \"\$d\" b 7 \"\${d#/dev/loop}\"
      losetup \"\$d\" ${file}
    fi
    echo \"\$d\" > ${file}.dev"
  local dev manifest
  dev=$(node "cat ${file}.dev")
  manifest=$(mktemp)
  curl -fsSL -o "${manifest}" "https://raw.githubusercontent.com/openebs/lvm-localpv/${version}/deploy/lvm-operator.yaml"
  echo "${manifest_sha256}  ${manifest}" | sha256sum -c --quiet - \
    || { echo "lvm-localpv: lvm-operator.yaml of ${version} is not the one checked" >&2; exit 1; }
  sed "s#openebs/lvm-driver:ci#${image}#" "${manifest}" | k apply -f - >/dev/null
  rm -f "${manifest}"
  k -n kube-system rollout status daemonset/openebs-lvm-node --timeout=300s >/dev/null
  k -n kube-system rollout status deployment/openebs-lvm-controller --timeout=300s >/dev/null
  plugin "vgs fleetvg >/dev/null 2>&1 || { pvcreate -q ${dev} && vgcreate -q fleetvg ${dev}; }" >&2
  k apply -f - >/dev/null <<'EOF'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: fleet-lvm}
provisioner: local.csi.openebs.io
parameters: {storage: lvm, volgroup: fleetvg, fsType: ext4}
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Delete
EOF
  echo fleet-lvm
}

down() {
  local dev pv
  dev=$(node "cat ${file}.dev 2>/dev/null" || true)
  # The volumes go first: their logical volumes hold the device.
  k delete pvc -A --field-selector=metadata.namespace!=kube-system --wait=false >/dev/null 2>&1 || true
  for _ in $(seq 1 60); do
    pv=$(k get pv -o jsonpath='{range .items[?(@.spec.storageClassName=="fleet-lvm")]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -c . || true)
    [ "${pv}" = 0 ] && break
    sleep 2
  done
  plugin "vgremove -fy fleetvg >/dev/null 2>&1; pvremove -fy ${dev} >/dev/null 2>&1" 2>/dev/null || true
  [ -z "${dev}" ] || node "losetup -d ${dev}" 2>/dev/null || true
  node "rm -f ${file} ${file}.dev" 2>/dev/null || true
}

case "${cmd}" in
  up) up ;;
  down) down ;;
  *) echo "usage: $0 up|down <profile> [size]" >&2; exit 2 ;;
esac
