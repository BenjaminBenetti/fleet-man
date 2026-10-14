#!/usr/bin/env bash
# itest: k8s
# Description: the merge gate's transport measurements (FLEET_ITEST_BENCH_REPORT names the report; without it the test passes saying it did not run), a cluster instance beside a Docker one with a TUI open: forward throughput down and up through the pooled port-forward and through the pod agent's forward stream (--half-close, which picks the default forward path), audio jitter beside a bulk download on each, `fleet exec` output against kubectl exec, a 48MB `fleet copy` each way, the exec round trip, attach latency (`fleet shell` on a daemon-issued credential, a tmux session, kubectl exec -it) and a page through each instance's browser proxy; on the plain link and, with FLEET_ITEST_WAN_ON / FLEET_ITEST_WAN_OFF (the nightly bench job's netem hooks), behind the WAN. It records; it judges nothing.
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
  tui_kill
  rm -rf "${workdir}"
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the cluster's data paths against Docker's"
[ -n "${FLEET_ITEST_BENCH_REPORT:-}" ] \
  || pass "not run: FLEET_ITEST_BENCH_REPORT is not set (integration/nightly/bench.sh sets it)"

report="${FLEET_ITEST_BENCH_REPORT}"
secs="${FLEET_ITEST_BENCH_SECONDS:-8}"
# record <metric> <value> <unit> — a row of the report (the 85x form, with
# no budget: these are measurements, read by the merge checklist).
record() {
  printf '%s\t%s\t%s\t-\t-\n' "$1" "$2" "$3" >> "${report}"
  info "$1 = $2 $3"
}
now_ms() { echo $(( $(date +%s%N) / 1000000 )); }
sub_fail() { printf '%s\n' "$*" >&2; exit 1; }
# median <values...> — the middle one (the lower of the two middles).
median() { printf '%s\n' "$@" | sort -n | awk '{ v[NR] = $1 } END { print v[int((NR + 1) / 2)] }'; }
wan_on() { sh -c "${FLEET_ITEST_WAN_ON}" || fail "FLEET_ITEST_WAN_ON failed"; wan=on; }
wan_off() { sh -c "${FLEET_ITEST_WAN_OFF}" || fail "FLEET_ITEST_WAN_OFF failed"; wan=""; }

setup_net_test
# Audio output on, this client's speaker selected (as 745, 815 and 851).
printf '{ "output_settings": { "enabled": true, "client": "itest-desk", "device": "pulse:itest_speaker" } }\n' > "${HOME}/.fleet/config.json.new"
mv "${HOME}/.fleet/config.json.new" "${HOME}/.fleet/config.json"
itest_config_written

fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"
out=$("${FLEET_BIN}" up "${FIXTURE_REPO_NAME}/dock" --backend devcontainer 2>&1) || fail "fleet up dock --backend devcontainer failed: ${out}"
dock="${FIXTURE_REPO_NAME}/dock"
pod=$(itest_k8s_pod "$(itest_container_id "${inst}")")

for ref in "${inst}" "${dock}"; do
  "${FLEET_BIN}" exec "${ref}" -- truncate -s 4G /tmp/bench-source.bin
  itest_net_serve "${ref}" http 17380
  itest_net_serve "${ref}" source 17390 0.0.0.0 /tmp/bench-source.bin
  itest_net_serve "${ref}" count 17391
  "${FLEET_BIN}" spawn-session "${ref}" poll >/dev/null
done
# A port only each instance serves, to tell their browser proxies apart.
itest_net_serve "${inst}" http 17382
itest_net_serve "${dock}" http 17383

# forward <label> <ref> [--half-close] — `fleet port-forward add` of the
# source, count and http ports; prints "<source> <count> <http>" (local).
forward() {
  local label="$1" ref="$2" s c h
  shift 2
  s=$(itest_net_free_port); c=$(itest_net_free_port); h=$(itest_net_free_port)
  "${FLEET_BIN}" port-forward add "${ref}" "$@" "${s}:17390" "${c}:17391" "${h}:17380" > "${workdir}/pf-${label}.log" 2>&1 &
  bg_pids+=("$!")
  itest_net_wait_accepts "${h}" 60 || fail "the ${label} forward never listened: $(cat "${workdir}/pf-${label}.log")"
  echo "${s} ${c} ${h}"
}
read -r pool_src pool_cnt pool_http <<< "$(forward pool "${inst}")"
read -r agent_src agent_cnt agent_http <<< "$(forward agent "${inst}" --half-close)"
read -r dock_src dock_cnt dock_http <<< "$(forward dock "${dock}")"
kube_http=$(itest_net_free_port)
itest_k8s_kubectl port-forward "pod/${pod}" "${kube_http}:17380" > "${workdir}/kpf.log" 2>&1 &
bg_pids+=("$!")
itest_net_wait_accepts "${kube_http}" 60 || fail "kubectl port-forward never listened: $(cat "${workdir}/kpf.log")"

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
PATH="${workdir}/bin:${PATH}" FLEET_MIC_CLIENT=itest-desk "${FLEET_BIN}" output attach > "${workdir}/attach.log" 2>&1 &
bg_pids+=("$!")
deadline=$(( $(date +%s) + $(_scale_timeout 180) ))
until grep -q "audio output source attached.*${inst}" "${HOME}/.fleet/fleet.log" 2>/dev/null; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the instance's output source never attached: $(cat "${workdir}/attach.log")"
  sleep 0.5
done

info "a TUI open (its pollers run), and each instance's browser proxy started from it"
launches="${workdir}/chrome-launches.log"
cat > "${workdir}/bin/google-chrome" <<STUB
#!/bin/sh
echo "\$*" >> "${launches}"
STUB
chmod +x "${workdir}/bin/google-chrome"
export PATH="${workdir}/bin:${PATH}"
tmux set-environment -g PATH "${PATH}" >/dev/null 2>&1 || true
tui_spawn
tui_wait_for "alpha" 30
tui_wait_for "dock" 30
tui_wait_for "○ idle" 60
# proxy_of <rows-down> — press b on that row; the proxy port the browser got.
proxy_of() {
  local before
  before=$(cat "${launches}" 2>/dev/null | wc -l)
  tui_send "$1"
  sleep 0.3
  tui_send b
  deadline=$(( $(date +%s) + $(_scale_timeout 300) ))
  until [ "$(cat "${launches}" 2>/dev/null | wc -l)" -gt "${before}" ]; do
    if tui_capture | grep -q "Browser proxy error"; then sub_fail "'b' failed: $(tui_capture)"; fi
    [ "$(date +%s)" -lt "${deadline}" ] || sub_fail "the browser was never launched: $(tui_capture)"
    sleep 1
  done
  sed -n "$(( before + 1 ))p" "${launches}" | grep -oE -- '--proxy-server=http://localhost:[0-9]+' | grep -oE '[0-9]+$'
}
proxy_alpha=$(proxy_of j) || fail "alpha's browser proxy (above)"
proxy_dock=$(proxy_of j) || fail "dock's browser proxy (above)"
[ -n "${proxy_alpha}" ] && [ -n "${proxy_dock}" ] || fail "a browser was launched without a proxy: $(cat "${launches}")"
# Which row each press reached: each proxy reaches its own instance's port.
proxy_reaches() { curl -sS --max-time 30 -x "http://127.0.0.1:$1" -o /dev/null "http://localhost:$2/health" 2>/dev/null; }
if ! proxy_reaches "${proxy_alpha}" 17382; then
  read -r proxy_alpha proxy_dock <<< "${proxy_dock} ${proxy_alpha}"
fi
proxy_reaches "${proxy_alpha}" 17382 || fail "neither browser proxy reaches alpha: $(cat "${launches}")"
proxy_reaches "${proxy_dock}" 17383 || fail "no browser proxy reaches dock: $(cat "${launches}")"

rate() { itest_net_client rate "$1" "$2" "${secs}"; }
# page_ms <curl proxy args...> -- <base url> — a page as 851 loads it: its
# HTML, then eight 128KiB assets six at a time; the best of three (ms).
page_ms() {
  local args=() base i run best=""
  while [ "$1" != -- ]; do args+=("$1"); shift; done
  base="$2"
  for run in 1 2 3; do
    local assets=() start t
    for i in 1 2 3 4 5 6 7 8; do assets+=(-o /dev/null "${base}/bytes/131072?a=${i}&r=${run}"); done
    start=$(now_ms)
    curl -sS --max-time 120 "${args[@]}" -o /dev/null "${base}/bytes/16384?r=${run}" || sub_fail "the page's HTML through ${args[*]} ${base} failed"
    curl -sS --max-time 120 --parallel --parallel-max 6 "${args[@]}" "${assets[@]}" || sub_fail "the page's assets through ${args[*]} ${base} failed"
    t=$(( $(now_ms) - start ))
    if [ -z "${best}" ] || [ "${t}" -lt "${best}" ]; then best="${t}"; fi
  done
  echo "${best}"
}
# exec_rtt_ms <ref> — the median of five `fleet exec <ref> -- true` (ms).
exec_rtt_ms() {
  local i s v=()
  for i in 1 2 3 4 5; do
    s=$(now_ms)
    "${FLEET_BIN}" exec "$1" -- true || sub_fail "fleet exec $1 failed"
    v+=("$(( $(now_ms) - s ))")
  done
  median "${v[@]}"
}
# attach_ms <how> <ref> — ms until a marker shows on an attach's terminal:
# "exec" a command through `fleet shell` (kubernetes: direct mode on the
# credential fleetd issues), "session" the poll tmux session, "kubectl" kubectl
# exec -it (the cluster's own floor). The median of three.
attach_ms() {
  local how="$1" ref="$2" i m v=()
  for i in 1 2 3; do
    m="$RANDOM$RANDOM"
    case "${how}" in
      exec) v+=("$(itest_net_client pty-wait 120 "MARK-${m}" -- "${FLEET_BIN}" shell "${ref}" -- sh -c "printf 'M%sK-%s\\n' AR ${m}; sleep 60")") || sub_fail "fleet shell ${ref} (above)" ;;
      session)
        "${FLEET_BIN}" exec-in-session "${ref}" poll "clear; printf 'M%sK-%s\\n' AR ${m}" >/dev/null || sub_fail "exec-in-session ${ref} failed"
        sleep 1
        v+=("$(itest_net_client pty-wait 120 "MARK-${m}" -- "${FLEET_BIN}" shell "${ref}" --session poll)") || sub_fail "fleet shell ${ref} --session poll (above)" ;;
      kubectl) v+=("$(itest_net_client pty-wait 120 "MARK-${m}" -- kubectl --context "${FLEET_ITEST_CLUSTER}" -n "${FLEET_ITEST_NAMESPACE}" exec -it "${pod}" -c dev -- sh -c "printf 'M%sK-%s\\n' AR ${m}; sleep 60")") || sub_fail "kubectl exec -it (above)" ;;
    esac
  done
  median "${v[@]}"
}
jitter_lines() { grep "audio output jitter.*instance=${inst} " "${HOME}/.fleet/fleet.log" 2>/dev/null || true; }
# jitter_beside <label> <source-port|""> — the worst p99 (ms) of the noise's
# jitter lines while one bulk download runs through that forward (none: quiet).
jitter_beside() {
  local label="$1" port="$2" mark tone dl
  mark=$(jitter_lines | wc -l)
  "${FLEET_BIN}" exec "${inst}" -- sh -c "head -c $(( (secs + 8) * 192000 )) /dev/urandom | pacat --raw --format=s16le --rate=48000 --channels=2" >/dev/null 2>&1 &
  tone=$!
  bg_pids+=("${tone}")
  if [ -n "${port}" ]; then
    sleep 3
    itest_net_client rate "${port}" down "${secs}" > "${workdir}/jitter-${label}.rate" 2>&1 &
    dl=$!
    wait "${dl}" || sub_fail "${label}: the download beside the noise failed: $(cat "${workdir}/jitter-${label}.rate")"
  fi
  wait "${tone}" || true
  deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
  until [ "$(jitter_lines | wc -l)" -gt "${mark}" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || sub_fail "${label}: no audio output jitter line after the noise"
    sleep 0.5
  done
  jitter_lines | sed -n "$(( mark + 1 )),\$p" | sed -E 's/.* p99_ms=([0-9]+).*/\1/' | sort -n | tail -n 1
}
head -c 50331648 /dev/urandom > "${workdir}/copy.bin"
# copy_ms <ref> <in|out> — a 48MB `fleet copy` into or out of the instance.
copy_ms() {
  local s
  s=$(now_ms)
  if [ "$2" = in ]; then
    "${FLEET_BIN}" copy "${workdir}/copy.bin" "$1:/tmp/bench-copy.bin" >/dev/null 2>&1 || sub_fail "fleet copy into $1 failed"
  else
    rm -f "${workdir}/copy-back.bin"
    "${FLEET_BIN}" copy "$1:/tmp/bench-copy.bin" "${workdir}/copy-back.bin" >/dev/null 2>&1 || sub_fail "fleet copy out of $1 failed"
  fi
  echo $(( $(now_ms) - s ))
}

# measure <prefix> <with-docker> — one pass; the Docker instance (local,
# never behind the WAN) only on the plain link.
measure() {
  local p="$1" v
  v=$(exec_rtt_ms "${inst}") || fail "exec round trip (above)"; record "${p}_exec_rtt_ms" "${v}" ms
  v=$(rate "${pool_src}" down) || fail "pool download (above)"; record "${p}_forward_pool_down_mbps" "${v}" MB/s
  v=$(rate "${agent_src}" down) || fail "agent download (above)"; record "${p}_forward_agent_down_mbps" "${v}" MB/s
  v=$(rate "${pool_cnt}" up) || fail "pool upload (above)"; record "${p}_forward_pool_up_mbps" "${v}" MB/s
  v=$(rate "${agent_cnt}" up) || fail "agent upload (above)"; record "${p}_forward_agent_up_mbps" "${v}" MB/s
  v=$(itest_net_client rate-cmd "${secs}" -- "${FLEET_BIN}" exec "${inst}" -- head -c 4294967296 /dev/zero) || fail "fleet exec output (above)"
  record "${p}_exec_out_fleet_mbps" "${v}" MB/s
  v=$(itest_net_client rate-cmd "${secs}" -- kubectl --context "${FLEET_ITEST_CLUSTER}" -n "${FLEET_ITEST_NAMESPACE}" exec "${pod}" -c dev -- head -c 4294967296 /dev/zero) || fail "kubectl exec output (above)"
  record "${p}_exec_out_kubectl_mbps" "${v}" MB/s
  v=$(copy_ms "${inst}" in) || fail "copy in (above)"; record "${p}_copy48_in_ms" "${v}" ms
  v=$(copy_ms "${inst}" out) || fail "copy out (above)"; record "${p}_copy48_out_ms" "${v}" ms
  v=$(attach_ms exec "${inst}") || fail "attach (above)"; record "${p}_attach_exec_ms" "${v}" ms
  v=$(attach_ms session "${inst}") || fail "attach to a session (above)"; record "${p}_attach_session_ms" "${v}" ms
  v=$(attach_ms kubectl "${inst}") || fail "kubectl attach (above)"; record "${p}_attach_kubectl_ms" "${v}" ms
  v=$(page_ms -x "http://127.0.0.1:${proxy_alpha}" -- "http://localhost:17380") || fail "page through alpha's proxy (above)"
  record "${p}_page_proxy_ms" "${v}" ms
  v=$(page_ms -- "http://127.0.0.1:${pool_http}") || fail "page through fleet's forward (above)"; record "${p}_page_forward_ms" "${v}" ms
  v=$(page_ms -- "http://127.0.0.1:${kube_http}") || fail "page through kubectl (above)"; record "${p}_page_kubectl_ms" "${v}" ms
  v=$(jitter_beside "${p}-quiet" "") || fail "quiet jitter (above)"; record "${p}_jitter_quiet_p99_ms" "${v}" ms
  v=$(jitter_beside "${p}-pool" "${pool_src}") || fail "jitter beside a pool download (above)"; record "${p}_jitter_pool_download_p99_ms" "${v}" ms
  v=$(jitter_beside "${p}-agent" "${agent_src}") || fail "jitter beside an agent download (above)"; record "${p}_jitter_agent_download_p99_ms" "${v}" ms
  [ "$2" = with-docker ] || return 0
  v=$(exec_rtt_ms "${dock}") || fail "docker exec round trip (above)"; record docker_exec_rtt_ms "${v}" ms
  v=$(rate "${dock_src}" down) || fail "docker download (above)"; record docker_forward_down_mbps "${v}" MB/s
  v=$(rate "${dock_cnt}" up) || fail "docker upload (above)"; record docker_forward_up_mbps "${v}" MB/s
  v=$(itest_net_client rate-cmd "${secs}" -- "${FLEET_BIN}" exec "${dock}" -- head -c 4294967296 /dev/zero) || fail "docker exec output (above)"
  record docker_exec_out_mbps "${v}" MB/s
  v=$(copy_ms "${dock}" in) || fail "docker copy in (above)"; record docker_copy48_in_ms "${v}" ms
  v=$(copy_ms "${dock}" out) || fail "docker copy out (above)"; record docker_copy48_out_ms "${v}" ms
  v=$(attach_ms exec "${dock}") || fail "docker attach (above)"; record docker_attach_exec_ms "${v}" ms
  v=$(attach_ms session "${dock}") || fail "docker session attach (above)"; record docker_attach_session_ms "${v}" ms
  v=$(page_ms -x "http://127.0.0.1:${proxy_dock}" -- "http://localhost:17380") || fail "page through dock's proxy (above)"
  record docker_page_proxy_ms "${v}" ms
}

info "on the plain link"
measure plain with-docker
if [ -n "${FLEET_ITEST_WAN_ON:-}" ] && [ -n "${FLEET_ITEST_WAN_OFF:-}" ]; then
  info "behind the WAN"
  wan_on
  measure wan cluster-only
  wan_off
fi
pass "measured the cluster's data paths beside Docker's (${report})"
