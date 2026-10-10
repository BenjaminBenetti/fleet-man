#!/usr/bin/env bash
# Description: the TUI's instance row shows live CPU (mcpu) and memory (MB) for a running instance, with a fleet total: both climb under load inside the instance and fall back once it ends.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
load_pids=""
itest_cleanup() {
  if [ -n "${load_pids}" ]; then
    kill ${load_pids} >/dev/null 2>&1 || true
    "${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- pkill -f '[i]test-stats-load' >/dev/null 2>&1 || true
  fi
  tui_kill
}
itest_begin

setup_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

# row_stats — "<mcpu> <MB>" from alpha's row ("  1234 mcpu   567.8 MB"), empty
# until the first stats sample lands.
row_stats() {
  { tui_capture || true; } | sed -nE '/[[:space:]]alpha[[:space:]].*mcpu/{
      s/.*[[:space:]]([0-9]+) mcpu[[:space:]]+([0-9]+)(\.[0-9]+)? MB.*/\1 \2/p
      q
    }'
}
# wait_stats <description> <cpu|mem> <-ge|-lt> <bound> [timeout_s] — poll
# alpha's row until its mcpu or MB figure compares to <bound> as asked.
wait_stats() {
  local what="$1" field="$2" op="$3" bound="$4" deadline v met s=""
  deadline=$(( $(date +%s) + $(_scale_timeout "${5:-60}") ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    s=$(row_stats)
    if [ -n "${s}" ]; then
      if [ "${field}" = cpu ]; then v=${s% *}; else v=${s#* }; fi
      met=""
      case "${op}" in
        -ge) [ "${v}" -ge "${bound}" ] && met=1 ;;
        -lt) [ "${v}" -lt "${bound}" ] && met=1 ;;
      esac
      if [ -n "${met}" ]; then
        info "${what}: ${s% *} mcpu, ${s#* } MB"
        return 0
      fi
    fi
    sleep 1
  done
  printf -- '--- screen ---\n%s\n--- end ---\n' "$(tui_capture)" >&2
  fail "alpha's row never showed ${what} (last: ${s:-no stats})"
}

info "Spawning TUI"
tui_spawn
tui_wait_for "alpha" 15 || fail "alpha's row never rendered"

wait_stats "stats at idle" cpu -ge 0
tui_assert_contains "Total:" "the fleet total line should render with the stats"
idle=$(row_stats)
idle_mem=${idle#* }

# fleet shows what the backend measures. A nested dockerd whose containers get
# no memory controller (cgroup v2 without nesting set up, as in older QA images)
# reports "0B / 0B" for every container, so there is no memory to show.
mem_measured=1
if itest_backend_is devcontainer; then
  docker_mem=$(docker stats --no-stream --format '{{.MemUsage}}' "$(itest_container_id "${inst}")")
  if [ "${docker_mem}" = "0B / 0B" ]; then
    mem_measured=""
    info "docker measures no memory for its containers here (${docker_mem}); skipping the memory checks"
  fi
fi

# Both load processes carry the word itest-stats-load in their argv, so they can
# be stopped inside the instance (stopping a host `fleet exec` client does not
# reach the process it started). They also end on their own after 90s.
info "loading the instance: one busy core and ~300 MB held"
"${FLEET_BIN}" exec "${inst}" -- sh -c 'end=$(( $(date +%s) + 90 )); while [ "$(date +%s)" -lt "${end}" ]; do :; done' itest-stats-load >/dev/null 2>&1 &
load_pids="$!"
"${FLEET_BIN}" exec "${inst}" -- perl -e '$x = "a" x (300 * 1024 * 1024); sleep 90' itest-stats-load >/dev/null 2>&1 &
load_pids="${load_pids} $!"

wait_stats "CPU under load" cpu -ge 400
if [ -n "${mem_measured}" ]; then
  wait_stats "memory under load" mem -ge $(( idle_mem + 250 ))
fi

info "ending the load"
kill ${load_pids} >/dev/null 2>&1 || true
load_pids=""
# [i]... matches the load's argv but not this pkill's own.
"${FLEET_BIN}" exec "${inst}" -- pkill -f '[i]test-stats-load' >/dev/null 2>&1 || true

wait_stats "CPU back down" cpu -lt 300
if [ -n "${mem_measured}" ]; then
  wait_stats "memory back down" mem -lt $(( idle_mem + 150 ))
fi

pass "TUI shows live CPU/memory stats that track load"
