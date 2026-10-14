#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster behind a WAN (FLEET_ITEST_WAN_ON / FLEET_ITEST_WAN_OFF shape the link to the node: the nightly netem job's 150ms round trip with 1% loss), an instance's audio stays smooth through bulk transfers (PCM jitter p99 under 60ms, FLEET_ITEST_WAN_JITTER_MS, while a 48MB fleet copy and a 50MB file of a synced folder cross; the jitter without them is recorded beside it), a page through fleet's port forward loads within 2x of the same page through kubectl port-forward on that link, and the poll and page times with and without the WAN are recorded; without the hooks it passes saying it did not run.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
source "$(dirname "$0")/../net/net.sh"
workdir=$(mktemp -d)
bg_pids=()
wan=""
itest_cleanup() {
  local p
  for p in "${bg_pids[@]}"; do kill "${p}" 2>/dev/null || true; done
  if [ -n "${wan}" ]; then sh -c "${FLEET_ITEST_WAN_OFF}" || true; fi
  rm -rf "${workdir}"
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: a cluster behind a WAN"
[ -n "${FLEET_ITEST_WAN_ON:-}" ] && [ -n "${FLEET_ITEST_WAN_OFF:-}" ] \
  || pass "not run: FLEET_ITEST_WAN_ON / FLEET_ITEST_WAN_OFF are not set (integration/nightly/netem.sh sets them)"

# "none" records the jitter without judging it (a lossy link: TCP holds a
# stream for each lost packet's retransmission, whatever runs beside it).
jitter_budget_ms="${FLEET_ITEST_WAN_JITTER_MS:-60}"
[ "${jitter_budget_ms}" != none ] || jitter_budget_ms=""
page_ratio_budget="${FLEET_ITEST_WAN_PAGE_RATIO:-2}"
report="${FLEET_ITEST_WAN_REPORT:-${workdir}/report.tsv}"
over_budget=""
# record <metric> <value> <unit> [<budget>] — as 850's report.
record() {
  local verdict="-"
  if [ -n "${4:-}" ]; then
    if awk -v v="$2" -v b="$4" 'BEGIN { exit !(v + 0 <= b + 0) }'; then verdict=ok; else verdict=over; over_budget="${over_budget} $1"; fi
  fi
  printf '%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "${4:--}" "${verdict}" >> "${report}"
  info "$1 = $2 $3${4:+ (budget $4: ${verdict})}"
}
wan_on() { sh -c "${FLEET_ITEST_WAN_ON}" || fail "FLEET_ITEST_WAN_ON failed"; wan=on; }
wan_off() { sh -c "${FLEET_ITEST_WAN_OFF}" || fail "FLEET_ITEST_WAN_OFF failed"; wan=""; }
now_ms() { echo $(( $(date +%s%N) / 1000000 )); }
# sub_fail — a failure inside a measurement run as $(...): it ends that
# subshell (the caller fails the test, so the result is written once).
sub_fail() { printf '%s\n' "$*" >&2; exit 1; }

setup_net_test
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/vscode false false '["/opt/data"]'
# Audio output on, this client's speaker selected (as 745 and 815).
printf '{ "output_settings": { "enabled": true, "client": "itest-desk", "device": "pulse:itest_speaker" } }\n' > "${HOME}/.fleet/config.json.new"
mv "${HOME}/.fleet/config.json.new" "${HOME}/.fleet/config.json"
itest_config_written

fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"
itest_net_serve "${inst}" http 17380
"${FLEET_BIN}" spawn-session "${inst}" poll >/dev/null

info "attach as the audio output client"
mkdir -p "${workdir}/bin"
cat > "${workdir}/bin/pactl" <<'STUB'
#!/bin/sh
if [ "$1" = list ] && [ "$2" = sinks ]; then
  printf 'Sink #0\n\tState: SUSPENDED\n\tName: itest_speaker\n\tDescription: Itest Speaker\n'
  exit 0
fi
exit 1
STUB
printf '#!/bin/sh\nexec cat >/dev/null\n' > "${workdir}/bin/paplay"
chmod +x "${workdir}/bin/pactl" "${workdir}/bin/paplay"
attach_log="${workdir}/attach.log"
PATH="${workdir}/bin:${PATH}" FLEET_MIC_CLIENT=itest-desk "${FLEET_BIN}" output attach > "${attach_log}" 2>&1 &
bg_pids+=("$!")
deadline=$(( $(date +%s) + $(_scale_timeout 180) ))
until grep -q "audio output source attached.*${inst}" "${HOME}/.fleet/fleet.log" 2>/dev/null; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the instance's output source never attached: $(cat "${attach_log}")"
  sleep 0.5
done

# jitter_during <label> <seconds> [bulk|quiet] — play noise in the instance
# for that long, with (bulk, the default) a 48MB copy and a 50MB file of the
# synced folder crossing beside it (both under way 3s into the noise), then
# print the worst p99 (ms) of the noise's "audio output jitter" lines in
# fleet.log.
head -c 50331648 /dev/urandom > "${workdir}/copy.bin"
host_data=$(itest_share_dir "${FIXTURE_REPO_NAME}" .mnt/opt/data)
mkdir -p "${host_data}"
jitter_lines() { grep "audio output jitter.*instance=${inst} " "${HOME}/.fleet/fleet.log" 2>/dev/null || true; }
jitter_during() {
  local label="$1" secs="$2" mark tone copy started took
  mark=$(jitter_lines | wc -l)
  "${FLEET_BIN}" exec "${inst}" -- sh -c "head -c $(( secs * 192000 )) /dev/urandom | pacat --raw --format=s16le --rate=48000 --channels=2" >/dev/null 2>&1 &
  tone=$!
  bg_pids+=("${tone}")
  if [ "${3:-bulk}" = bulk ]; then
    sleep 3
    started=$(date +%s)
    "${FLEET_BIN}" copy "${workdir}/copy.bin" "${inst}:/tmp/copy-${label}.bin" > "${workdir}/copy-${label}.log" 2>&1 &
    copy=$!
    head -c 52428800 /dev/urandom > "${host_data}/sync-${label}.bin"
    itest_share_sync "${FIXTURE_REPO_NAME}"
    wait "${copy}" || sub_fail "${label}: the 48MB copy failed: $(cat "${workdir}/copy-${label}.log")"
    took=$(( $(date +%s) - started ))
    # Its output is the p99: what it tells goes to stderr.
    [ "${took}" -lt "$(( secs - 3 ))" ] || info "${label}: the transfers took ${took}s, longer than the ${secs}s of noise" >&2
  fi
  wait "${tone}" || true
  deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
  until [ "$(jitter_lines | wc -l)" -gt "${mark}" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || sub_fail "${label}: no audio output jitter line after the noise: $(grep -E 'audio output' "${HOME}/.fleet/fleet.log" | tail -3)"
    sleep 0.5
  done
  "${FLEET_BIN}" exec "${inst}" -- rm -f "/tmp/copy-${label}.bin"
  rm -f "${host_data}/sync-${label}.bin"
  jitter_lines | sed -n "$(( mark + 1 )),\$p" | sed -E 's/.* p99_ms=([0-9]+).*/\1/' | sort -n | tail -n 1
}

# page_ms <port> — a page through a local port: its HTML and eight 128KiB
# assets, six at a time, as a browser fetches them (ms).
page_ms() {
  local start urls=() i
  urls+=("http://127.0.0.1:$1/bytes/16384")
  for i in 1 2 3 4 5 6 7 8; do urls+=("http://127.0.0.1:$1/bytes/131072?a=${i}"); done
  local assets=()
  for i in "${urls[@]:1}"; do assets+=(-o /dev/null "${i}"); done
  start=$(now_ms)
  curl -sS --max-time 120 -o "${workdir}/page.html" "${urls[0]}" || sub_fail "the page's HTML through :$1 failed"
  curl -sS --max-time 120 --parallel --parallel-max 6 "${assets[@]}" || sub_fail "the page's assets through :$1 failed"
  echo $(( $(now_ms) - start ))
}
# best_page_ms <port> — the fastest of three loads.
best_page_ms() { { page_ms "$1"; page_ms "$1"; page_ms "$1"; } | sort -n | head -n 1; }

fleet_port=$(itest_net_free_port)
"${FLEET_BIN}" port-forward add "${inst}" "${fleet_port}:17380" > "${workdir}/pf.log" 2>&1 &
bg_pids+=("$!")
itest_net_wait_accepts "${fleet_port}" 60 || fail "fleet's port forward never listened: $(cat "${workdir}/pf.log")"
pod=$(itest_k8s_pod "$(itest_container_id "${inst}")")
kube_port=$(itest_net_free_port)
itest_k8s_kubectl port-forward "pod/${pod}" "${kube_port}:17380" > "${workdir}/kpf.log" 2>&1 &
bg_pids+=("$!")
itest_net_wait_accepts "${kube_port}" 60 || fail "kubectl port-forward never listened: $(cat "${workdir}/kpf.log")"

# poll_ms — p50 and p95 (ms) of 20 reads of a session's screen: the TUI's
# poll, over the pod's agent session.
poll_ms() {
  local i s e
  for i in $(seq 20); do
    s=$(now_ms)
    "${FLEET_BIN}" read-session "${inst}" poll >/dev/null || sub_fail "read-session failed"
    e=$(now_ms)
    echo $(( e - s ))
  done | sort -n | awk '{ v[NR] = $1 } END { printf "%d %d\n", v[int(NR * 0.5) + 1], v[int(NR * 0.95)] }'
}

info "on the plain link"
polls=$(poll_ms) || fail "the session polls failed (above)"
read -r p50 p95 <<< "${polls}"
record local_poll_p50_ms "${p50}" ms
record local_poll_p95_ms "${p95}" ms
v=$(best_page_ms "${fleet_port}") || fail "the page through fleet failed (above)"
record local_page_fleet_ms "${v}" ms
v=$(best_page_ms "${kube_port}") || fail "the page through kubectl failed (above)"
record local_page_kubectl_ms "${v}" ms
v=$(jitter_during local-quiet 20 quiet) || fail "the quiet jitter measurement failed (above)"
record local_quiet_jitter_p99_ms "${v}" ms
v=$(jitter_during local 40) || fail "the bulk jitter measurement failed (above)"
record local_bulk_jitter_p99_ms "${v}" ms

info "behind the WAN"
wan_on
rtt_start=$(now_ms)
"${FLEET_BIN}" exec "${inst}" -- true
record wan_exec_roundtrip_ms "$(( $(now_ms) - rtt_start ))" ms
polls=$(poll_ms) || fail "the session polls failed (above)"
read -r p50 p95 <<< "${polls}"
record wan_poll_p50_ms "${p50}" ms
record wan_poll_p95_ms "${p95}" ms
fleet_ms=$(best_page_ms "${fleet_port}") || fail "the page through fleet failed behind the WAN (above)"
kube_ms=$(best_page_ms "${kube_port}") || fail "the page through kubectl failed behind the WAN (above)"
record wan_page_fleet_ms "${fleet_ms}" ms
record wan_page_kubectl_ms "${kube_ms}" ms
record wan_page_fleet_vs_kubectl "$(awk -v f="${fleet_ms}" -v k="${kube_ms}" 'BEGIN { printf "%.2f\n", f / (k > 0 ? k : 1) }')" x "${page_ratio_budget}"
v=$(jitter_during wan-quiet 30 quiet) || fail "the quiet jitter measurement failed behind the WAN (above)"
record wan_quiet_jitter_p99_ms "${v}" ms
v=$(jitter_during wan 120) || fail "the bulk jitter measurement failed behind the WAN (above)"
record wan_bulk_jitter_p99_ms "${v}" ms ${jitter_budget_ms:+"${jitter_budget_ms}"}
wan_off

[ -z "${over_budget}" ] || fail "over budget:${over_budget} (report: $(tr '\t' ' ' < "${report}" | tr '\n' ';'))"
pass "behind the WAN: audio jitter and page loads within budget"
