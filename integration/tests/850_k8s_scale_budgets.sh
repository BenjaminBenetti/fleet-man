#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, FLEET_ITEST_SCALE lightweight instances keep fleetd within its scale budgets (RSS under 600 MB, under 4 open files and at most 2 API server long-running requests per pod once the bulk sessions idle out), every agent is back within 90s of a fleetd restart (the reconnect storm, whose exec rate is recorded), and past 150 instances the connection warns; without FLEET_ITEST_SCALE (the nightly scale job sets it) it passes saying it did not run.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
workdir=$(mktemp -d)
itest_cleanup() { rm -rf "${workdir}"; }
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: fleetd's budgets at scale on a cluster"
n="${FLEET_ITEST_SCALE:-0}"
case "${n}" in
  '' | *[!0-9]*) fail "FLEET_ITEST_SCALE=${n} is not a number of instances" ;;
esac
[ "${n}" -gt 0 ] || pass "not run: FLEET_ITEST_SCALE is not set (integration/nightly/scale.sh sets it)"

# The budgets (plan: durability-ops "Scale budgets"), held at any scale.
rss_budget_mb="${FLEET_ITEST_SCALE_RSS_MB:-600}"
fds_per_pod=4
long_running_per_pod=2
reconnect_budget_s="${FLEET_ITEST_SCALE_RECONNECT_S:-90}"
many=150
parallel="${FLEET_ITEST_SCALE_PARALLEL:-8}"
# A pod as light as fleet's own processes allow: no tools, no lifecycle
# commands, a small request (the default 500m/2Gi would fit a few per node).
image="${FLEET_ITEST_SCALE_IMAGE:-docker.io/library/alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc}"
report="${FLEET_ITEST_SCALE_REPORT:-${workdir}/report.tsv}"

# record <metric> <value> <unit> [<budget>] — one line of the run's report
# (name, value, unit, budget, ok|over|-), also printed.
over_budget=""
record() {
  local verdict="-"
  if [ -n "${4:-}" ]; then
    if awk -v v="$2" -v b="$4" 'BEGIN { exit !(v + 0 <= b + 0) }'; then verdict=ok; else verdict=over; over_budget="${over_budget} $1"; fi
  fi
  printf '%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "${4:--}" "${verdict}" >> "${report}"
  info "$1 = $2 $3${4:+ (budget $4: ${verdict})}"
}

# metric_sum <awk condition> — the sum of the API server's metric lines
# matching it.
metric_sum() {
  itest_k8s_kubectl get --raw /metrics 2>/dev/null | awk "/^$1\\{/ && $2 { n += \$NF } END { printf \"%d\\n\", n }"
}
# connects — the pod exec/port-forward/attach requests the API server holds
# open (each agent session and bulk session is one).
connects() { metric_sum apiserver_longrunning_requests '/verb="CONNECT"/ && /resource="pods"/'; }
agent_execs() { metric_sum apiserver_longrunning_requests '/verb="CONNECT"/ && /subresource="exec"/'; }
# watches_by — the API server's open WATCHes, one "<resource>/<scope> <n>"
# line each: a watch of one object (scope resource) is the kubelet's of a
# pod's Secret or ConfigMap, a list watch (cluster, namespace) an informer's.
watches_by() {
  itest_k8s_kubectl get --raw /metrics 2>/dev/null | awk '/^apiserver_longrunning_requests\{/ && /verb="WATCH"/ {
    match($0, /resource="[^"]*"/); r = substr($0, RSTART + 10, RLENGTH - 11)
    match($0, /scope="[^"]*"/); s = substr($0, RSTART + 7, RLENGTH - 8)
    n[r "/" s] += $NF
  } END { for (k in n) print k, n[k] }' | sort
}
# watch_growth <before> <after> — the watches that grew, "<resource>/<scope>
# +<n>" each, and their total last.
watch_growth() {
  join -a 2 -e 0 -o 0,1.2,2.2 <(printf '%s\n' "$1") <(printf '%s\n' "$2") | awk '
    $3 - $2 > 0 { printf "%s +%d ", $1, $3 - $2; if ($1 ~ /\/resource$/) obj += $3 - $2; else list += $3 - $2 }
    END { printf "\n%d %d\n", list, obj }'
}
exec_total() { metric_sum apiserver_request_total '/verb="CONNECT"/ && /subresource="exec"/'; }
server_pid() { pgrep -f "${FLEET_BIN} server" 2>/dev/null | head -n1 || true; }
# alive <pid> — the process runs (a zombie waiting for its reaper does not).
alive() { local st; st=$(ps -o stat= -p "$1" 2>/dev/null || true); [ -n "${st}" ] && [ "${st#Z}" = "${st}" ]; }
running_count() { "${FLEET_BIN}" ls 2>/dev/null | awk -v f="${FIXTURE_REPO_NAME}" '$1 == f && $3 == "running" { n++ } END { print n + 0 }'; }

mkdir -p "${workdir}/fixture/.devcontainer"
printf '{ "name": "fleet scale fixture", "image": "%s" }\n' "${image}" > "${workdir}/fixture/.devcontainer/devcontainer.json"
FIXTURE_SRC="${workdir}/fixture" setup_test
"${FLEET_BIN}" cluster set itest resources.cpu_request=5m resources.memory_request=24Mi disk_size=1Gi >/dev/null
# What fleetd holds with no instance yet (its logs, sockets, listeners and
# the connection's informers): the per-pod budgets count what comes on top.
watch_base=$(watches_by)
base_pid=$(server_pid)
[ -n "${base_pid}" ] || fail "no fleetd is running"
fds_base=$(find "/proc/${base_pid}/fd" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l | tr -d ' ')

info "create ${n} instances, ${parallel} at a time (the first alone: it makes the fleet's share)"
start=$(date +%s)
fleet_up s1 >"${workdir}/up-1.log" 2>&1 || fail "the first create failed: $(tail -5 "${workdir}/up-1.log")"
if [ "${n}" -gt 1 ]; then
  seq 2 "${n}" | xargs -P "${parallel}" -I{} sh -c '"$1" up "$2/s$3" --repo "$4" >"$5/up-$3.log" 2>&1 || echo "s$3" >> "$5/failed"' \
    _ "${FLEET_BIN}" "${FIXTURE_REPO_NAME}" {} "${FIXTURE_REPO_URL}" "${workdir}"
fi
[ ! -s "${workdir}/failed" ] || fail "$(wc -l < "${workdir}/failed") creates failed, first $(head -1 "${workdir}/failed"): $(tail -5 "${workdir}/up-$(head -1 "${workdir}/failed" | tr -d s).log")"
running=$(running_count)
assert_equals "${n}" "${running}" "instances running"
record instances "${n}" count
record create_seconds "$(( $(date +%s) - start ))" s

info "every agent connected, then the bulk sessions idle out"
deadline=$(( $(date +%s) + $(_scale_timeout 300) ))
until [ "$(agent_execs)" -ge "${n}" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "only $(agent_execs) of ${n} agent sessions are open"
  sleep 2
done
# A bulk session closes after a minute without a stream (DefaultBulkIdle).
# What is still open after that counts against the budget.
deadline=$(( $(date +%s) + $(_scale_timeout 180) ))
while [ "$(connects)" -gt "${n}" ] && [ "$(date +%s)" -lt "${deadline}" ]; do
  sleep 5
done
pid=$(server_pid)
[ -n "${pid}" ] || fail "no fleetd is running"
rss_kb=$(awk '/^VmRSS:/ { print $2 }' "/proc/${pid}/status")
fds=$(find "/proc/${pid}/fd" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l | tr -d ' ')
long_running=$(connects)
record fleetd_rss_mb "$(( rss_kb / 1024 ))" MB "${rss_budget_mb}"
record fleetd_rss_per_pod_kb "$(( rss_kb / n ))" KB
[ "${pid}" = "${base_pid}" ] || fail "fleetd restarted during the creates (${base_pid} -> ${pid})"
record fleetd_fds "${fds}" count "$(( fds_base + fds_per_pod * n ))"
record fleetd_fds_per_pod "$(awk -v f="${fds}" -v b="${fds_base}" -v n="${n}" 'BEGIN { printf "%.2f\n", (f - b) / n }')" count "${fds_per_pod}"
record long_running_connects "${long_running}" count "$(( long_running_per_pod * n ))"
growth=$(watch_growth "${watch_base}" "$(watches_by)")
info "watches that grew with the pods: $(printf '%s\n' "${growth}" | head -n 1)"
read -r list_growth object_growth <<< "$(printf '%s\n' "${growth}" | tail -n 1)"
# fleetd's own watches are informers of the connection, not of each pod.
record long_running_list_watches "${list_growth}" count 20
record long_running_object_watches "${object_growth}" count

if [ "${n}" -gt "${many}" ]; then
  info "more than ${many} instances: the connection warns"
  "${FLEET_BIN}" cluster test itest >/dev/null 2>&1 || true
  assert_contains "$("${FLEET_BIN}" cluster ls itest 2>&1)" "${n} instances on this connection" "the many-instances warning"
fi

info "the reconnect storm: fleetd restarts with ${n} pods running"
kill -TERM "${pid}"
deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
while alive "${pid}"; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "fleetd ${pid} did not stop on SIGTERM"
  sleep 0.5
done
# Its sessions end with it.
deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
until [ "$(agent_execs)" -lt "${n}" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the stopped fleetd's agent sessions stayed open: $(agent_execs)"
  sleep 0.5
done
before=$(exec_total)
restart=$(date +%s%N)
"${FLEET_BIN}" ls >/dev/null
peak=0
last=${before}
last_at=${restart}
deadline=$(( $(date +%s) + $(_scale_timeout 600) ))
while :; do
  open=$(agent_execs)
  now=$(date +%s%N)
  total=$(exec_total)
  rate=$(awk -v d="$(( total - last ))" -v ns="$(( now - last_at ))" 'BEGIN { printf "%d\n", (ns > 0 ? d * 1e9 / ns : 0) + 0.5 }')
  [ "${rate}" -le "${peak}" ] || peak=${rate}
  last=${total}
  last_at=${now}
  [ "${open}" -lt "${n}" ] || break
  [ "$(date +%s)" -lt "${deadline}" ] || fail "only ${open} of ${n} agents were back 10 minutes after the restart"
  sleep 1
done
reconnect_ms=$(( (now - restart) / 1000000 ))
record reconnect_seconds "$(awk -v ms="${reconnect_ms}" 'BEGIN { printf "%.1f\n", ms / 1000 }')" s "${reconnect_budget_s}"
record reconnect_exec_upgrades "$(( last - before ))" count
record reconnect_peak_exec_per_s "${peak}" "1/s"
assert_equals "${n}" "$(running_count)" "instances running after the restart"

[ -z "${over_budget}" ] || fail "over budget:${over_budget} (report: $(tr '\t' ' ' < "${report}" | tr '\n' ';'))"
pass "${n} instances within fleetd's scale budgets; reconnected in $(( reconnect_ms / 1000 ))s"
