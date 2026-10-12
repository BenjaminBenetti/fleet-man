#!/usr/bin/env bash
# itest: k8s
# Description: the merge gate's create and poll measurements (FLEET_ITEST_BENCH_REPORT names the report; without it the test passes saying it did not run), the two the optional extensions are gated on: interactive create latency on the cluster (the first instance, a second one, a clone) against Docker's (first, second), each with its steps' times from the creation log; and what an open TUI's pollers cost per running instance (fleetd's CPU, the pod agents', the cluster node's, the API server traffic; dockerd's and its shims' for Docker), from windows with and without a TUI. It records; it judges nothing.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
workdir=$(mktemp -d)
bg_pids=()
itest_cleanup() {
  local p
  for p in "${bg_pids[@]}"; do kill "${p}" 2>/dev/null || true; done
  tui_kill
  rm -rf "${workdir}"
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the cluster's create and poll costs against Docker's"
[ -n "${FLEET_ITEST_BENCH_REPORT:-}" ] \
  || pass "not run: FLEET_ITEST_BENCH_REPORT is not set (integration/nightly/bench.sh sets it)"

report="${FLEET_ITEST_BENCH_REPORT}"
window="${FLEET_ITEST_BENCH_POLL_SECONDS:-60}"
record() {
  printf '%s\t%s\t%s\t-\t-\n' "$1" "$2" "$3" >> "${report}"
  info "$1 = $2 $3"
}
now_ms() { echo $(( $(date +%s%N) / 1000000 )); }
clk_tck=$(getconf CLK_TCK)

setup_test

# timed_up <prefix> <fleet up|clone args...> — the command's wall time, and
# each step's from its creation log (the "==> <step>" lines, stamped as they
# land): <prefix>_ms, <prefix>_step_<step>_ms.
timed_up() {
  local prefix="$1" inst log stamps start took
  shift
  case "$1" in
    clone) inst="$3" ;;
    *) inst="$2" ;;
  esac
  inst="${inst#*/}"
  log="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-${inst}.log"
  stamps="${workdir}/${prefix}.stamps"
  tail -n +1 -F "${log}" 2>/dev/null | perl -MTime::HiRes=time -ne '$| = 1; printf "%d %s", time * 1000, $_' > "${stamps}" &
  local tailer=$!
  bg_pids+=("${tailer}")
  start=$(now_ms)
  "${FLEET_BIN}" "$@" > "${workdir}/${prefix}.out" 2>&1 || fail "fleet $* failed: $(cat "${workdir}/${prefix}.out")"
  took=$(( $(now_ms) - start ))
  sleep 1
  kill "${tailer}" 2>/dev/null || true
  record "${prefix}_ms" "${took}" ms
  # Each step lasts until the next one (the last until the command ended);
  # a step that comes back (one line per detail) adds up.
  awk -v end="$(( start + took ))" -v p="${prefix}" '
    function add(n, d) { if (!(n in dur)) order[++k] = n; dur[n] += d }
    / ==> / && !/==> fleet / && !/==> done/ {
      t = $1; sub(/^[0-9]+ ==> /, ""); sub(/:.*/, ""); gsub(/[^A-Za-z0-9]+/, "_"); name = tolower($0)
      if (prev != "") add(prev, t - at)
      prev = name; at = t
    }
    END {
      if (prev != "") add(prev, end - at)
      for (i = 1; i <= k; i++) printf "%s_step_%s_ms\t%d\n", p, order[i], dur[order[i]]
    }' "${stamps}" \
  | while IFS="$(printf '\t')" read -r metric v; do record "${metric}" "${v}" ms; done
}

info "creates: Docker first (its base image pull is the cold one), then the cluster's"
timed_up docker_first up "${FIXTURE_REPO_NAME}/dock1" --repo "${FIXTURE_REPO_URL}" --backend devcontainer
timed_up docker_second up "${FIXTURE_REPO_NAME}/dock2" --repo "${FIXTURE_REPO_URL}" --backend devcontainer
timed_up k8s_first up "${FIXTURE_REPO_NAME}/alpha" --repo "${FIXTURE_REPO_URL}"
timed_up k8s_second up "${FIXTURE_REPO_NAME}/beta" --repo "${FIXTURE_REPO_URL}"
timed_up k8s_clone clone "${FIXTURE_REPO_NAME}/beta" gamma
k8s_insts=(alpha beta gamma)
docker_insts=(dock1 dock2)

# --- what the pollers cost ---
server_pid() { pgrep -f "${FLEET_BIN} server" 2>/dev/null | head -n1 || true; }
# ticks <pid...> — the processes' CPU (utime + stime + their reaped
# children's), in clock ticks.
ticks() {
  local p sum=0 t
  for p in "$@"; do
    t=$(awk '{ print $14 + $15 + $16 + $17 }' "/proc/${p}/stat" 2>/dev/null || echo 0)
    sum=$(( sum + t ))
  done
  echo "${sum}"
}
docker_pids() { pgrep -x dockerd; pgrep -x containerd; pgrep -f containerd-shim; }
# node_usec — the cluster node container's CPU (its cgroup), or "" where it
# cannot be read.
node="${FLEET_ITEST_CLUSTER}"
node_usec() { docker exec "${node}" awk '$1 == "usage_usec" { print $2 }' /sys/fs/cgroup/cpu.stat 2>/dev/null || true; }
# agent_ticks — the pod agents' CPU (each running cluster instance's
# `fleet pod-agent serve`), in clock ticks, read through kubectl so fleet's
# own sessions do not count it.
agent_ticks() {
  local i pod sum=0 t
  for i in "${k8s_insts[@]}"; do
    pod=$(itest_k8s_pod "$(itest_container_id "${FIXTURE_REPO_NAME}/${i}")") || continue
    t=$(itest_k8s_kubectl exec "${pod}" -c dev -- sh -c 'for p in /proc/[0-9]*; do case "$(tr "\0" " " < $p/cmdline 2>/dev/null)" in *"pod-agent serve"*) awk "{ s += \$14 + \$15 + \$16 + \$17 } END { print s }" $p/stat;; esac; done' 2>/dev/null | awk '{ s += $1 } END { print s + 0 }')
    sum=$(( sum + ${t:-0} ))
  done
  echo "${sum}"
}
# api_bytes — bytes fleetd's TCP connections sent and received (the API
# server's, on this host the only TCP peers it has besides the TUI's none).
api_bytes() {
  local pid
  pid=$(server_pid)
  ss -tinpH 2>/dev/null | awk -v pid="pid=${pid}," '
    /^ESTAB|^[A-Z]/ { mine = index($0, pid) > 0; next }
    mine { for (i = 1; i <= NF; i++) if ($i ~ /^bytes_(acked|received):/) { split($i, kv, ":"); s += kv[2] } }
    END { print s + 0 }'
}
# poll_window <label> <instances> — <window>s of the daemon as it is now:
# fleetd's CPU and API traffic, and the label's own side (k8s: the node and
# the pod agents; docker: dockerd, containerd and the shims).
poll_window() {
  local label="$1" n="$2" pid t0 t1 a0 a1 b0 b1 n0 n1 d0 d1
  pid=$(server_pid)
  [ -n "${pid}" ] || fail "no fleetd running"
  t0=$(ticks "${pid}"); b0=$(api_bytes)
  case "${label}" in
    k8s*) a0=$(agent_ticks); n0=$(node_usec) ;;
    docker*) d0=$(ticks $(docker_pids)) ;;
  esac
  sleep "${window}"
  t1=$(ticks "${pid}"); b1=$(api_bytes)
  record "poll_${label}_fleetd_cpu_ms_per_s" "$(awk -v d=$(( t1 - t0 )) -v hz="${clk_tck}" -v w="${window}" 'BEGIN { printf "%.1f\n", d * 1000 / hz / w }')" ms/s
  # (A connection that closed in the window takes its bytes with it: a
  # count that went down is none.)
  record "poll_${label}_api_bytes_per_s" "$(( b1 > b0 ? (b1 - b0) / window : 0 ))" B/s
  case "${label}" in
    k8s*)
      a1=$(agent_ticks); n1=$(node_usec)
      record "poll_${label}_agents_cpu_ms_per_s" "$(awk -v d=$(( a1 - a0 )) -v hz="${clk_tck}" -v w="${window}" 'BEGIN { printf "%.1f\n", d * 1000 / hz / w }')" ms/s
      if [ -n "${n0}" ] && [ -n "${n1}" ]; then
        record "poll_${label}_node_cpu_ms_per_s" "$(awk -v d=$(( n1 - n0 )) -v w="${window}" 'BEGIN { printf "%.1f\n", d / 1000 / w }')" ms/s
      fi
      ;;
    docker*)
      d1=$(ticks $(docker_pids))
      record "poll_${label}_dockerd_cpu_ms_per_s" "$(awk -v d=$(( d1 - d0 )) -v hz="${clk_tck}" -v w="${window}" 'BEGIN { printf "%.1f\n", d * 1000 / hz / w }')" ms/s
      ;;
  esac
  record "poll_${label}_instances" "${n}" instances
}
# tui_window <label> <instances> — poll_window with and without a TUI open.
tui_window() {
  tui_kill
  sleep 10
  poll_window "$1_no_tui" "$2"
  tui_spawn
  tui_wait_for "itest-fleet" 30
  sleep 15
  poll_window "$1_tui" "$2"
  tui_kill
}

info "the pollers' cost: cluster instances running, Docker's stopped"
for i in "${docker_insts[@]}"; do "${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/${i}" >/dev/null; done
for i in "${k8s_insts[@]}"; do "${FLEET_BIN}" spawn-session "${FIXTURE_REPO_NAME}/${i}" work >/dev/null; done
tui_window k8s "${#k8s_insts[@]}"

info "the pollers' cost: Docker instances running, the cluster's stopped"
for i in "${k8s_insts[@]}"; do "${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/${i}" >/dev/null; done
for i in "${docker_insts[@]}"; do
  "${FLEET_BIN}" start "${FIXTURE_REPO_NAME}/${i}" >/dev/null
  "${FLEET_BIN}" spawn-session "${FIXTURE_REPO_NAME}/${i}" work >/dev/null
done
tui_window docker "${#docker_insts[@]}"

info "the pollers' cost with nothing running"
for i in "${docker_insts[@]}"; do "${FLEET_BIN}" stop "${FIXTURE_REPO_NAME}/${i}" >/dev/null; done
tui_window idle 0

pass "measured creates and the pollers' cost per instance (${report})"
