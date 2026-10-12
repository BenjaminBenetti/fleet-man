#!/usr/bin/env bash
# selinux-k3s.sh — fleet on an SELinux-enforcing node: a throwaway KVM
# virtual machine (AlmaLinux 9 GenericCloud, enforcing) running k3s with
# --selinux (container-selinux, k3s-selinux), reached through qemu's user
# network (ssh and the API server forwarded to this host's loopback), then
# a fleet's share across its pods (817), its sync (824) and the SELinux
# checks of 853 (one MCS level per fleet, siblings sharing files, the
# relabel time of a full share). Images the CLI did not build are pulled by
# the node itself (passthrough), so only image fixtures run.
#
# Needs /dev/kvm, qemu-system-x86_64, qemu-img, cloud-localds and ssh. The
# base image (~600 MB, checked) is kept in FLEET_NIGHTLY_CACHE (default
# ~/.cache/fleet-nightly); the VM's disk (a copy-on-write overlay, a few GB)
# in FLEET_NIGHTLY_VM_DIR (default <out>/vm). FLEET_NIGHTLY_VM_MEMORY (6144)
# and FLEET_NIGHTLY_VM_CPUS (4) size it.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

nightly_begin selinux-k3s
nightly_require qemu-system-x86_64 qemu-img cloud-localds ssh ssh-keygen scp kubectl docker devcontainer tmux curl sha256sum
[ -w /dev/kvm ] || { nightly_say "/dev/kvm is not writable"; exit 2; }

image_name=AlmaLinux-9-GenericCloud-9.8-20260810.x86_64.qcow2
image_sha256=6bdab6376d46d42e4203ace3733efafc7c5d37c7cb443a6cc74750097002d74b
k3s_version="${FLEET_NIGHTLY_K3S_VERSION:-v1.37.1+k3s1}"
cache="${FLEET_NIGHTLY_CACHE:-${HOME}/.cache/fleet-nightly}"
vm="${FLEET_NIGHTLY_VM_DIR:-${NIGHTLY_OUT}/vm}"
ctx=selinux-k3s
mkdir -p "${cache}" "${vm}"

base="${cache}/${image_name}"
if ! echo "${image_sha256}  ${base}" | sha256sum -c --quiet - 2>/dev/null; then
  nightly_say "downloading ${image_name}"
  curl -fsSL -o "${base}.part" "https://repo.almalinux.org/almalinux/9/cloud/x86_64/images/${image_name}" \
    || curl -fsSL -o "${base}.part" "https://vault.almalinux.org/9.8/cloud/x86_64/images/${image_name}"
  echo "${image_sha256}  ${base}.part" | sha256sum -c --quiet - || { nightly_say "${image_name} is not the one checked"; exit 2; }
  mv "${base}.part" "${base}"
fi

free_port() { python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'; }
ssh_port=$(free_port)
api_port=$(free_port)
rm -f "${vm}/id" "${vm}/id.pub"
ssh-keygen -q -t ed25519 -N '' -C fleet-nightly -f "${vm}/id"
cat > "${vm}/user-data" <<EOF
#cloud-config
hostname: fleet-selinux
users:
  - name: fleet
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys: [$(cat "${vm}/id.pub")]
ssh_pwauth: false
growpart: {mode: auto, devices: ['/']}
EOF
printf 'instance-id: fleet-selinux\nlocal-hostname: fleet-selinux\n' > "${vm}/meta-data"
cloud-localds "${vm}/seed.iso" "${vm}/user-data" "${vm}/meta-data"
rm -f "${vm}/disk.qcow2"
qemu-img create -q -f qcow2 -F qcow2 -b "${base}" "${vm}/disk.qcow2" 40G
qemu-system-x86_64 -enable-kvm -cpu host -smp "${FLEET_NIGHTLY_VM_CPUS:-4}" -m "${FLEET_NIGHTLY_VM_MEMORY:-6144}" \
  -drive "file=${vm}/disk.qcow2,if=virtio" -drive "file=${vm}/seed.iso,if=virtio,format=raw" \
  -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:${ssh_port}-:22,hostfwd=tcp:127.0.0.1:${api_port}-:6443" \
  -device virtio-net-pci,netdev=n0 -display none -serial "file:${NIGHTLY_OUT}/vm-serial.log" \
  -pidfile "${vm}/qemu.pid" -daemonize
teardown() {
  [ -n "${FLEET_NIGHTLY_KEEP:-}" ] && { nightly_say "keeping the VM (ssh -p ${ssh_port} -i ${vm}/id fleet@127.0.0.1)"; return 0; }
  pkill -f "[f]leet server" 2>/dev/null || true
  kubectl config delete-context "${ctx}" >/dev/null 2>&1 || true
  kubectl config delete-cluster "${ctx}" >/dev/null 2>&1 || true
  kubectl config delete-user "${ctx}" >/dev/null 2>&1 || true
  [ -f "${vm}/qemu.pid" ] && kill "$(cat "${vm}/qemu.pid")" 2>/dev/null || true
  rm -f "${vm}/disk.qcow2" "${vm}/seed.iso" "${vm}/id" "${vm}/id.pub" "${vm}/qemu.pid" "${vm}/kubeconfig" "${vm}/user-data" "${vm}/meta-data"
}
trap teardown EXIT

vm_ssh() { ssh -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -i "${vm}/id" -p "${ssh_port}" fleet@127.0.0.1 "$@"; }
nightly_say "waiting for the VM's ssh"
for _ in $(seq 1 120); do vm_ssh true 2>/dev/null && break; sleep 3; done
vm_ssh true || { tail -n 40 "${NIGHTLY_OUT}/vm-serial.log" >&2; nightly_say "the VM never answered ssh"; exit 2; }
enforce=$(vm_ssh getenforce)
[ "${enforce}" = Enforcing ] || { nightly_say "the VM's SELinux is ${enforce}, not Enforcing"; exit 2; }

nightly_say "installing k3s ${k3s_version} with --selinux"
vm_ssh sudo bash -s > "${NIGHTLY_OUT}/vm-provision.log" 2>&1 <<EOF
set -euxo pipefail
# sudo's secure_path leaves out /usr/local/bin, where k3s goes.
export PATH="/usr/local/bin:\${PATH}"
dnf -y -q install container-selinux policycoreutils-python-utils tar
curl -sfL https://get.k3s.io -o /root/k3s-install.sh
INSTALL_K3S_VERSION='${k3s_version}' INSTALL_K3S_EXEC='--selinux --write-kubeconfig-mode=644 --tls-san=127.0.0.1' sh /root/k3s-install.sh
for i in \$(seq 1 60); do k3s kubectl get nodes 2>/dev/null | grep -q ' Ready' && break; sleep 5; done
k3s kubectl get nodes -o wide
rpm -q k3s-selinux container-selinux
grep -i selinux /var/lib/rancher/k3s/agent/etc/containerd/config.toml* || true
EOF
vm_ssh sudo cat /etc/rancher/k3s/k3s.yaml \
  | sed -e "s#https://127.0.0.1:6443#https://127.0.0.1:${api_port}#" -e "s/: default$/: ${ctx}/" > "${vm}/kubeconfig"
merged=$(mktemp)
KUBECONFIG="${HOME}/.kube/config:${vm}/kubeconfig" kubectl config view --flatten > "${merged}"
mkdir -p "${HOME}/.kube"
mv "${merged}" "${HOME}/.kube/config"
chmod 600 "${HOME}/.kube/config"
kubectl --context "${ctx}" wait --for=condition=Ready nodes --all --timeout=300s >&2
kubectl --context "${ctx}" -n kube-system rollout status deployment/coredns --timeout=300s >&2
kubectl --context "${ctx}" get nodes -o wide >&2
nightly_note "- AlmaLinux 9.8 (${enforce}), k3s ${k3s_version} --selinux, $(vm_ssh rpm -q container-selinux)"

nightly_itests "${ctx}" 817-818 || nightly_dump "${ctx}" share
NIGHTLY_ITEST_LABEL=sync nightly_itests "${ctx}" 824-825 || nightly_dump "${ctx}" sync
NIGHTLY_ITEST_LABEL=selinux nightly_itests "${ctx}" 853-854 FLEET_ITEST_SELINUX=1 FLEET_ITEST_SELINUX_REPORT="${NIGHTLY_OUT}/selinux.tsv" \
  || nightly_dump "${ctx}" selinux
[ ! -s "${NIGHTLY_OUT}/selinux.tsv" ] || nightly_budgets "${NIGHTLY_OUT}/selinux.tsv" "SELinux" || true
vm_ssh sudo ausearch -m avc -ts recent > "${NIGHTLY_OUT}/avc.log" 2>&1 || true
nightly_end
