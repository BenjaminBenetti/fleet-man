#!/usr/bin/env bash
# bench.sh — the merge gate's measurements, on a throwaway minikube whose
# node's link can be put behind FLEET_NIGHTLY_WAN_DELAY each way (default
# 75ms: a 150ms round trip) with tc netem: tests/855_k8s_gate_transport_bench.sh
# (forward throughput on the pooled port-forward against the pod agent's
# stream, audio jitter beside each, exec, copy, attach and page times, on
# the plain link and behind the WAN, beside a Docker instance) and
# tests/856_k8s_gate_create_poll_bench.sh (create latency and what an open
# TUI's pollers cost per instance, against Docker's). It records, into
# budgets.tsv and the summary, and judges nothing: the merge checklist and
# the optional extensions read the numbers. Needs tc, sudo and sch_netem.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

nightly_begin bench
nightly_require minikube kubectl docker devcontainer tmux tc sudo curl ping python3
profile=fleet-bench
delay="${FLEET_NIGHTLY_WAN_DELAY:-75ms}"
nightly_minikube "${profile}" --memory=6g --cpus=4
trap 'sudo tc qdisc del dev "${NIGHTLY_WAN_VETH:-none}" root 2>/dev/null || true; nightly_minikube_delete "${profile}"' EXIT

nightly_wan "${profile}" "${delay}"
rtt=$(nightly_wan_rtt)
nightly_note "- WAN: ${delay} each way; ping to the node ${rtt} ms with it on"

report="${NIGHTLY_OUT}/bench.tsv"
: > "${report}"
nightly_itests "${profile}" 855-857 FLEET_ITEST_WAN_ON="$(nightly_wan_on)" FLEET_ITEST_WAN_OFF="${NIGHTLY_WAN_OFF}" \
  FLEET_ITEST_BENCH_REPORT="${report}" || nightly_dump "${profile}" bench
nightly_budgets "${report}" "the merge gate's measurements (plain link, and behind a ${delay}x2 WAN)" || true
nightly_end
