#!/usr/bin/env bash
# lib.sh — what the nightly jobs (integration/nightly/*.sh) share; sourced.
#
# A job makes its own throwaway cluster, runs itests or Go tests against it,
# leaves what it measured and, for a failure, the cluster's state in its
# output folder, and deletes the cluster. It runs where the itests run: a
# GitHub runner (.github/workflows/nightly-k8s.yml) or the QA container
# (DEVELOPMENT.md "Nightly jobs"), never a dev machine's own session.
#
# Inputs (environment):
#   FLEET_BIN, FLEET_POD_BINARY_<ARCH>, FLEET_POD_TMUX_<ARCH>  the binaries
#       under test (one build); unset, run.sh builds fleet and the pod build
#   FLEET_NIGHTLY_OUT  where each job's folder goes (default ./nightly-out); a
#       run replaces its job's folder
#   FLEET_NIGHTLY_KEEP=1  keep the cluster when the job ends (debugging)
# Output, in <out>/<job>/: summary.md (also appended to
# $GITHUB_STEP_SUMMARY), budgets.tsv (metric, value, unit, budget, verdict),
# itests-<range>.log, artifacts/ (each failed test's FLEET_ITEST_ARTIFACTS
# dump) and dump-<label>/ (the cluster after a failed step).

NIGHTLY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${NIGHTLY_DIR}/../.." && pwd)"
NIGHTLY_FAILED=""

# nightly_begin <job> — the job's output folder, and the summary on exit.
nightly_begin() {
  NIGHTLY_JOB="$1"
  NIGHTLY_OUT="${FLEET_NIGHTLY_OUT:-${REPO_ROOT}/nightly-out}/${NIGHTLY_JOB}"
  # A run's folder holds only its own results: a local rerun into the same
  # output folder must not report the last run's failures or measurements.
  rm -rf "${NIGHTLY_OUT:?}"
  mkdir -p "${NIGHTLY_OUT}/artifacts"
  : > "${NIGHTLY_OUT}/budgets.tsv"
  NIGHTLY_START=$(date +%s)
  printf '## nightly %s\n\n' "${NIGHTLY_JOB}" > "${NIGHTLY_OUT}/summary.md"
  nightly_say "${NIGHTLY_JOB}: output in ${NIGHTLY_OUT}"
}

nightly_say() { printf '[nightly] %s\n' "$*" >&2; }

# nightly_note <markdown line> — a line of the job's summary.
nightly_note() { printf '%s\n' "$*" >> "${NIGHTLY_OUT}/summary.md"; }

# nightly_fail <what> — the job fails, after its other steps.
nightly_fail() {
  NIGHTLY_FAILED="${NIGHTLY_FAILED}${NIGHTLY_FAILED:+, }$1"
  nightly_say "FAILED: $1"
  nightly_note "- **FAILED**: $1"
}

# nightly_require <command...> — each must be on PATH.
nightly_require() {
  local c
  for c in "$@"; do
    command -v "${c}" >/dev/null 2>&1 || { nightly_say "missing tool: ${c}"; exit 2; }
  done
}

# nightly_memory_check <GiB> — the machine must have that much memory
# available for the job's cluster (a runner too small fails at once).
nightly_memory_check() {
  local avail
  avail=$(awk '/MemAvailable/ { printf "%d\n", $2 / 1048576 }' /proc/meminfo)
  [ "${avail}" -ge "$1" ] || { nightly_say "needs ${1} GiB of memory, ${avail} GiB available"; exit 2; }
}

# nightly_itests <context> <range> [VAR=value...] — the kubernetes itests of
# <range> on <context>, with the variables given; their log and failure
# artifacts go to the job's folder. A failure fails the job, not the step.
nightly_itests() {
  local ctx="$1" range="$2" log rc=0
  shift 2
  log="${NIGHTLY_OUT}/itests-${range}${NIGHTLY_ITEST_LABEL:+-${NIGHTLY_ITEST_LABEL}}.log"
  nightly_say "itests ${range} on ${ctx}${*:+ ($*)}"
  # The jobs run with pipefail: the pipeline fails with run.sh.
  if ! (
    cd "${REPO_ROOT}"
    env "$@" FLEET_ITEST_BACKEND=kubernetes FLEET_ITEST_CLUSTER="${ctx}" FLEET_ITEST_RANGE="${range}" \
      FLEET_ITEST_ARTIFACTS="${NIGHTLY_OUT}/artifacts" ./integration/run.sh
  ) 2>&1 | tee "${log}"; then
    rc=1
  fi
  local summary not_run
  summary=$(grep -E '^==> Summary:|^Failed:' "${log}" | sed 's/\x1b\[[0-9;]*m//g' | tr '\n' ' ')
  nightly_note "- itests ${range}${*:+ (\`$*\`)}: ${summary:-no summary}"
  # A test whose cluster this job did not give it passes saying it did not
  # run: named here, as a pass is not proof it ran.
  not_run=$(sed 's/\x1b\[[0-9;]*m//g' "${log}" | awk '
    /^==> RUN / { t = $3 }
    /^\[pass\]/ && /not run/ { sub(/^\[pass\] +/, ""); printf "%s%s (%s)", sep, t, $0; sep = "; " }')
  [ -z "${not_run}" ] || nightly_note "  - passed without running: ${not_run}"
  if [ "${rc}" != 0 ] || grep -q '^Failed:' "${log}"; then
    nightly_fail "itests ${range}"
    return 1
  fi
  # A job's range names the tests it is for: none there is a mistake.
  if grep -qE 'Empty shard|No kubernetes tests in shard' "${log}"; then
    nightly_fail "itests ${range}: no tests in the range"
    return 1
  fi
}

# nightly_budgets <report.tsv> <title> — the measurements of a run (as the
# 85x itests write them) into the job's budgets.tsv and summary; one over its
# budget fails the job.
nightly_budgets() {
  local tsv="$1" title="$2"
  [ -s "${tsv}" ] || { nightly_fail "${title}: no measurements"; return 1; }
  cat "${tsv}" >> "${NIGHTLY_OUT}/budgets.tsv"
  {
    printf '\n### %s\n\n| metric | value | budget | |\n|---|---|---|---|\n' "${title}"
    awk -F'\t' '{ printf "| %s | %s %s | %s | %s |\n", $1, $2, $3, $4, $5 }' "${tsv}"
    printf '\n'
  } >> "${NIGHTLY_OUT}/summary.md"
  if awk -F'\t' '$5 == "over" { bad = 1 } END { exit !bad }' "${tsv}"; then
    nightly_fail "${title}: over budget ($(awk -F'\t' '$5 == "over" { printf "%s ", $1 }' "${tsv}"))"
    return 1
  fi
}

# nightly_dump <context> <label> — the cluster's state, into dump-<label>/.
nightly_dump() {
  local ctx="$1" dir="${NIGHTLY_OUT}/dump-$2"
  mkdir -p "${dir}"
  nightly_say "dumping ${ctx} into ${dir}"
  kubectl --context "${ctx}" get all,pvc,pv,storageclass,leases,networkpolicies -A -o wide > "${dir}/objects.txt" 2>&1 || true
  kubectl --context "${ctx}" get events -A --sort-by=.lastTimestamp > "${dir}/events.txt" 2>&1 || true
  kubectl --context "${ctx}" describe nodes > "${dir}/nodes.txt" 2>&1 || true
  kubectl --context "${ctx}" describe pods -A -l app.kubernetes.io/managed-by=fleet > "${dir}/fleet-pods.txt" 2>&1 || true
  local pod
  for pod in $(kubectl --context "${ctx}" get pods -A -l app.kubernetes.io/managed-by=fleet \
    -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null); do
    kubectl --context "${ctx}" logs -n "${pod%%/*}" "${pod#*/}" --all-containers --prefix --tail=2000 > "${dir}/log-${pod//\//_}.txt" 2>&1 || true
  done
  kubectl --context "${ctx}" get pods -A -o wide > "${dir}/pods.txt" 2>&1 || true
}

# nightly_minikube <profile> <flags...> — a throwaway minikube; its context
# is the profile's name.
nightly_minikube() {
  local profile="$1"
  shift
  nightly_say "minikube ${profile}: $*"
  # shellcheck disable=SC2068
  MINIKUBE_WANTUPDATENOTIFICATION=false minikube start -p "${profile}" --driver=docker --wait=all $@ \
    > "${NIGHTLY_OUT}/minikube-${profile}.log" 2>&1 \
    || { tail -40 "${NIGHTLY_OUT}/minikube-${profile}.log" >&2; nightly_say "minikube ${profile} did not start"; exit 2; }
  kubectl --context "${profile}" get nodes -o wide >&2
}

# nightly_minikube_delete <profile>
nightly_minikube_delete() {
  [ -n "${FLEET_NIGHTLY_KEEP:-}" ] && { nightly_say "keeping minikube $1"; return 0; }
  minikube delete -p "$1" >/dev/null 2>&1 || true
}

# nightly_wan <profile> <delay> — tc netem on both ends of the veth between
# this host and minikube <profile>'s node, as hooks for the WAN itests
# (FLEET_ITEST_WAN_ON / FLEET_ITEST_WAN_OFF): sets NIGHTLY_WAN_VETH and
# NIGHTLY_WAN_OFF (the command that clears the link); `nightly_wan_on [loss]`
# prints the command that shapes it, <delay> each way and the loss given.
# Needs tc, sudo and the kernel's sch_netem; the job's EXIT trap clears it
# (sudo tc qdisc del dev "${NIGHTLY_WAN_VETH}" root).
nightly_wan() {
  local profile="$1" index
  NIGHTLY_WAN_PROFILE="${profile}"
  NIGHTLY_WAN_DELAY="$2"
  index=$(docker exec "${profile}" cat /sys/class/net/eth0/iflink)
  NIGHTLY_WAN_VETH=$(ip -o link | awk -F': ' -v i="${index}" '$1 == i { print $2 }' | cut -d@ -f1)
  [ -n "${NIGHTLY_WAN_VETH}" ] || { nightly_say "no veth on this host has index ${index} (the node's peer)"; exit 2; }
  NIGHTLY_WAN_OFF="docker exec ${profile} tc qdisc del dev eth0 root 2>/dev/null; sudo tc qdisc del dev ${NIGHTLY_WAN_VETH} root 2>/dev/null; true"
}
nightly_wan_on() {
  echo "docker exec ${NIGHTLY_WAN_PROFILE} tc qdisc replace dev eth0 root netem delay ${NIGHTLY_WAN_DELAY}${1:+ loss $1} && sudo tc qdisc replace dev ${NIGHTLY_WAN_VETH} root netem delay ${NIGHTLY_WAN_DELAY}${1:+ loss $1}"
}

# nightly_wan_rtt [loss] — the ping time (ms) to the node with the link shaped
# so; the link is left clear.
nightly_wan_rtt() {
  local node_ip rtt
  node_ip=$(kubectl --context "${NIGHTLY_WAN_PROFILE}" get node "${NIGHTLY_WAN_PROFILE}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
  sh -c "$(nightly_wan_on "${1:-}")" || { nightly_say "tc netem failed (is sch_netem available?)"; exit 2; }
  rtt=$(ping -c 5 -q "${node_ip}" | awk -F/ '/^rtt/ { print $5 }')
  sh -c "${NIGHTLY_WAN_OFF}"
  echo "${rtt}"
}

# nightly_end — the job's verdict: 0 unless a step failed.
nightly_end() {
  nightly_note ""
  nightly_note "took $(( ($(date +%s) - NIGHTLY_START) / 60 )) min"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    cat "${NIGHTLY_OUT}/summary.md" >> "${GITHUB_STEP_SUMMARY}"
  fi
  if [ -n "${NIGHTLY_FAILED}" ]; then
    nightly_say "${NIGHTLY_JOB} FAILED: ${NIGHTLY_FAILED}"
    return 1
  fi
  nightly_say "${NIGHTLY_JOB} passed"
}
