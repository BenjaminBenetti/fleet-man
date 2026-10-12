#!/usr/bin/env bash
# calico.sh — fleet on an overlay network that enforces NetworkPolicies: a
# throwaway minikube with calico at a 1450-byte pod MTU (an overlay on a
# 1500-byte underlay, checked from a new pod), then DinD at that MTU and
# pulling through the fleet's image mirror (714, 718), the deb cache (716),
# and 808-815: DooD, sibling DNS, the cloud metadata block calico enforces
# (811), the DooD image mirror, Buildkit permissions, audio pacing.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

nightly_begin calico
nightly_require minikube kubectl docker devcontainer tmux
profile=fleet-calico
mtu="${FLEET_NIGHTLY_CALICO_MTU:-1450}"
nightly_minikube "${profile}" --memory=6g --cpus=4 --cni=calico
trap 'nightly_minikube_delete "${profile}"' EXIT
k() { kubectl --context "${profile}" "$@"; }

k -n kube-system patch configmap calico-config --type merge -p "{\"data\":{\"veth_mtu\":\"${mtu}\"}}" >/dev/null
k -n kube-system rollout restart daemonset/calico-node >/dev/null
k -n kube-system rollout status daemonset/calico-node --timeout=300s >&2
k run fleet-mtu-probe --image=docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e \
  --restart=Never --command -- cat /sys/class/net/eth0/mtu >/dev/null
for _ in $(seq 1 90); do
  case "$(k get pod fleet-mtu-probe -o jsonpath='{.status.phase}' 2>/dev/null)" in Succeeded | Failed) break ;; esac
  sleep 2
done
got=$(k logs fleet-mtu-probe 2>/dev/null || true)
k delete pod fleet-mtu-probe --wait=false >/dev/null 2>&1 || true
[ "${got}" = "${mtu}" ] || { nightly_say "a new pod's MTU is '${got}', not ${mtu}"; exit 2; }
nightly_note "- calico, pod MTU ${got}"

nightly_itests "${profile}" 714-719 || nightly_dump "${profile}" mtu
NIGHTLY_ITEST_LABEL=calico nightly_itests "${profile}" 808-816 || nightly_dump "${profile}" calico
nightly_end
