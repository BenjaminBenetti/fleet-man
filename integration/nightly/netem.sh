#!/usr/bin/env bash
# netem.sh — fleet behind a WAN: a throwaway minikube whose node's link gets
# FLEET_NIGHTLY_WAN_DELAY each way (default 75ms: a 150ms round trip), with
# tc netem on both ends of the node's veth, while
# tests/851_k8s_wan_budgets.sh measures audio jitter beside bulk transfers,
# page loads and polls with and without it: once without loss, where the
# jitter budget (p99 under 60ms) holds, and once with
# FLEET_NIGHTLY_WAN_LOSS each way (default 0.5%: about 1% a round trip),
# where it is recorded (each lost packet holds a TCP stream for its
# retransmission, a round trip or more). Needs tc, sudo and the kernel's
# sch_netem.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

nightly_begin netem
nightly_require minikube kubectl docker devcontainer tmux tc sudo curl ping
profile=fleet-netem
delay="${FLEET_NIGHTLY_WAN_DELAY:-75ms}"
loss="${FLEET_NIGHTLY_WAN_LOSS:-0.5%}"
nightly_minikube "${profile}" --memory=6g --cpus=4
trap 'sudo tc qdisc del dev "${veth:-none}" root 2>/dev/null || true; nightly_minikube_delete "${profile}"' EXIT

# The node's eth0 and its peer on this host (the docker network's veth).
index=$(docker exec "${profile}" cat /sys/class/net/eth0/iflink)
veth=$(ip -o link | awk -F': ' -v i="${index}" '$1 == i { print $2 }' | cut -d@ -f1)
[ -n "${veth}" ] || { nightly_say "no veth on this host has index ${index} (the node's peer)"; exit 2; }
# shape_on [<loss>] — the hooks' commands for the link with that loss each way.
shape_on() { echo "docker exec ${profile} tc qdisc replace dev eth0 root netem delay ${delay}${1:+ loss $1} && sudo tc qdisc replace dev ${veth} root netem delay ${delay}${1:+ loss $1}"; }
off="docker exec ${profile} tc qdisc del dev eth0 root 2>/dev/null; sudo tc qdisc del dev ${veth} root 2>/dev/null; true"
node_ip=$(kubectl --context "${profile}" get node "${profile}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
sh -c "$(shape_on "${loss}")" || { nightly_say "tc netem failed (is sch_netem available?)"; exit 2; }
rtt=$(ping -c 5 -q "${node_ip}" | awk -F/ '/^rtt/ { print $5 }')
sh -c "${off}"
nightly_note "- WAN: ${delay} each way; ping to the node ${rtt} ms with it on"

for run in lossless lossy; do
  report="${NIGHTLY_OUT}/wan-${run}.tsv"
  : > "${report}"
  if [ "${run}" = lossless ]; then
    on=$(shape_on "") budget=60 title="behind a ${delay}x2 WAN"
  else
    on=$(shape_on "${loss}") budget=none title="behind a ${delay}x2 WAN losing ${loss} each way"
  fi
  NIGHTLY_ITEST_LABEL="${run}" nightly_itests "${profile}" 851-852 FLEET_ITEST_WAN_ON="${on}" FLEET_ITEST_WAN_OFF="${off}" \
    FLEET_ITEST_WAN_JITTER_MS="${budget}" FLEET_ITEST_WAN_REPORT="${report}" || nightly_dump "${profile}" "wan-${run}"
  nightly_budgets "${report}" "${title}" || true
done
nightly_end
