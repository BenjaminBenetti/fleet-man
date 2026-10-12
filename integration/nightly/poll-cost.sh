#!/usr/bin/env bash
# poll-cost.sh — what bound files cost a side without inotify (macOS, the
# BSDs), which scans its roots again every 2s: one pass over a host folder
# of 5000 entries holding 20 bound files (BenchmarkPollOfBoundFiles) must
# take under FLEET_NIGHTLY_POLL_BUDGET_MS (100: 5% of the poll interval).
# The nightly workflow runs it on macOS; elsewhere it measures that host.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

nightly_begin "poll-cost-$(uname -s | tr '[:upper:]' '[:lower:]')"
nightly_require go
budget="${FLEET_NIGHTLY_POLL_BUDGET_MS:-100}"
log="${NIGHTLY_OUT}/bench.txt"
(cd "${REPO_ROOT}" && go test -run '^$' -bench '^BenchmarkPollOfBoundFiles$' -benchtime 50x ./internal/sharesync) | tee "${log}"
ns=$(awk '/^BenchmarkPollOfBoundFiles/ { print $3; exit }' "${log}")
[ -n "${ns}" ] || { nightly_fail "the benchmark gave no result"; nightly_end; exit 1; }
ms=$(awk -v ns="${ns}" 'BEGIN { printf "%.1f\n", ns / 1e6 }')
verdict=$(awk -v ms="${ms}" -v b="${budget}" 'BEGIN { print (ms + 0 <= b + 0) ? "ok" : "over" }')
printf 'poll_pass_ms_%s\t%s\tms\t%s\t%s\n' "$(uname -s)" "${ms}" "${budget}" "${verdict}" > "${NIGHTLY_OUT}/poll.tsv"
nightly_budgets "${NIGHTLY_OUT}/poll.tsv" "a poll pass over 20 bound files in a 5000-entry folder ($(uname -s))" || true
nightly_end
