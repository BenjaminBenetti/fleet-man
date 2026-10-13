#!/usr/bin/env bash
# Integration test driver for fleet-man.
#
# - Builds the fleet binary to a temp location (unless FLEET_BIN is set).
# - Runs every test in integration/tests in alphabetical order, on the
#   FLEET_ITEST_BACKEND backend (devcontainer by default; kubernetes runs only
#   the tests marked "# itest: k8s", see common.sh "Test markers").
# - Between tests: runs teardown to kill any leftover containers / state.
# - Exits 0 only if every test passes.

set -euo pipefail

INTEGRATION_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${INTEGRATION_DIR}/.." && pwd)"

# Colors
if [ -t 1 ]; then
  BOLD='\033[1m'; GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'; RESET='\033[0m'
else
  BOLD=''; GREEN=''; RED=''; YELLOW=''; RESET=''
fi

say() { printf "%b==>%b %s\n" "${BOLD}" "${RESET}" "$*"; }

# FLEET_ITEST_BSD_SHIMS=1 — prepend integration/bsd-shims to PATH so host
# shell-outs see macOS/BSD command behavior on a GNU/Linux runner (see
# bsd-shims/README.md). Catches the #231 bug class (e.g. `sleep infinity`)
# across the full suite without Mac hardware. Container-side commands are
# unaffected: containers run their own Linux userland.
if [ -n "${FLEET_ITEST_BSD_SHIMS:-}" ]; then
  export PATH="${INTEGRATION_DIR}/bsd-shims:${PATH}"
  say "FLEET_ITEST_BSD_SHIMS set: host PATH now fronts BSD-behavior shims"
fi

# 1. Build fleet binary unless caller provided one. On a cluster the daemon
#    also serves its pods the pod build of the same source; it is built beside
#    it unless FLEET_POD_BINARY_<ARCH> names one (it must be the same build).
if [ -z "${FLEET_BIN:-}" ]; then
  build_dir="$(mktemp -d)"
  FLEET_BIN="${build_dir}/fleet"
  say "Building fleet -> ${FLEET_BIN}"
  (cd "${REPO_ROOT}" && go build -o "${FLEET_BIN}" ./cmd/fleet)
  if [ "${FLEET_ITEST_BACKEND:-}" = "kubernetes" ]; then
    pod_arch=$(go env GOARCH)
    pod_var="FLEET_POD_BINARY_$(printf '%s' "${pod_arch}" | tr '[:lower:]' '[:upper:]')"
    if [ -z "${!pod_var:-}" ]; then
      say "Building the pod build -> ${build_dir}/fleet-pod-linux-${pod_arch}"
      (cd "${REPO_ROOT}" && CGO_ENABLED=0 GOOS=linux go build -tags nok8s -o "${build_dir}/fleet-pod-linux-${pod_arch}" ./cmd/fleet)
      export "${pod_var}=${build_dir}/fleet-pod-linux-${pod_arch}"
    fi
  fi
fi
export FLEET_BIN
say "Using fleet binary: ${FLEET_BIN}"
"${FLEET_BIN}" --help >/dev/null || { printf "%b[fatal]%b fleet binary failed to run\n" "${RED}" "${RESET}" >&2; exit 2; }

# common.sh provides teardown_test, the test-marker helpers and the backend
# (FLEET_ITEST_BACKEND, validated there). Sourced only now: it defaults
# FLEET_BIN, which step 1 must see unset to build.
# shellcheck disable=SC1091
source "${INTEGRATION_DIR}/common.sh"

# 2. Preflight: docker + devcontainer CLI must be available and usable.
#    FLEET_ITEST_NO_DOCKER=1 skips the preflight and (below) restricts the
#    run to tests marked "# itest: no-docker" — the host-side subset (CLI
#    surface, TUI dialogs, daemon lifecycle, automation CRUD) that never
#    creates an instance. This is what lets the suite run on GitHub's
#    hosted macOS runners, whose Apple Silicon VMs cannot run Docker.
if [ -z "${FLEET_ITEST_NO_DOCKER:-}" ]; then
  if ! command -v docker >/dev/null 2>&1; then
    printf "%b[fatal]%b docker CLI not on PATH\n" "${RED}" "${RESET}" >&2
    exit 2
  fi
  if ! docker info >/dev/null 2>&1; then
    printf "%b[fatal]%b docker daemon not reachable\n" "${RED}" "${RESET}" >&2
    exit 2
  fi
  if ! command -v devcontainer >/dev/null 2>&1; then
    printf "%b[fatal]%b devcontainer CLI not on PATH (npm i -g @devcontainers/cli)\n" "${RED}" "${RESET}" >&2
    exit 2
  fi

  # 3. Pre-pull each fixture image once so individual tests are not waiting
  #    on image pulls. The agent fixture is used by the startup-script tests
  #    (claude / codex install) and is a different image from the default.
  for fixture_dir in "${INTEGRATION_DIR}/fixture" "${INTEGRATION_DIR}/fixture-agent"; do
    dc_json="${fixture_dir}/.devcontainer/devcontainer.json"
    [ -f "${dc_json}" ] || continue
    fixture_image=$(grep -oE '"image":\s*"[^"]+"' "${dc_json}" | sed -E 's/.*"([^"]+)"$/\1/')
    if [ -n "${fixture_image}" ]; then
      say "Pre-pulling fixture image: ${fixture_image}"
      docker pull -q "${fixture_image}" >/dev/null || true
    fi
  done
else
  say "FLEET_ITEST_NO_DOCKER set: skipping docker preflight, running docker-free tests only"
fi

# Backend preflight (kubernetes: kubectl reaches FLEET_ITEST_CLUSTER, defaulted
# to the current context). It prints its own [fatal] line.
_be_preflight || exit 2
if [ "${FLEET_ITEST_BACKEND}" = "kubernetes" ]; then
  say "Backend: kubernetes (context ${FLEET_ITEST_CLUSTER})"
else
  say "Backend: ${FLEET_ITEST_BACKEND}"
fi

# 4. Iterate tests.
shopt -s nullglob
tests=("${INTEGRATION_DIR}"/tests/*.sh)
shopt -u nullglob
if [ "${#tests[@]}" -eq 0 ]; then
  printf "%b[fatal]%b no tests found in %s\n" "${RED}" "${RESET}" "${INTEGRATION_DIR}/tests" >&2
  exit 2
fi

# Optional sharding: FLEET_ITEST_RANGE="START-END" keeps only tests whose
# numeric filename prefix N satisfies START <= N < END (END exclusive so
# hundred-boundaries don't double-run). An empty END means no upper bound, so
# the top shard ("600-") always catches new high-numbered tests instead of
# silently dropping them. CI fans the suite across matrix machines by range so
# the slowest shard — not the whole suite — sets the wall-clock. An empty result
# is a legitimately empty shard (exits 0), distinct from the empty-glob fatal above.
if [ -n "${FLEET_ITEST_RANGE:-}" ]; then
  range_start="10#${FLEET_ITEST_RANGE%-*}"
  range_end="${FLEET_ITEST_RANGE#*-}"
  shard=()
  for test_file in "${tests[@]}"; do
    prefix=$(basename "${test_file}" | grep -oE '^[0-9]+' || true)
    [ -n "${prefix}" ] || continue
    n="$((10#${prefix}))"
    [ "${n}" -ge "$((range_start))" ] || continue
    [ -z "${range_end}" ] || [ "${n}" -lt "$((10#${range_end}))" ] || continue
    shard+=("${test_file}")
  done
  # Same bash-3.2 constraint as the no-docker filter below: check the
  # length before expanding the possibly-empty array, and make the
  # documented empty-shard exit explicit.
  say "Shard ${FLEET_ITEST_RANGE}: running ${#shard[@]} test(s)"
  if [ "${#shard[@]}" -eq 0 ]; then
    say "Empty shard — nothing to do"
    exit 0
  fi
  tests=("${shard[@]}")
fi

# No-docker mode: keep only tests that opt in with a "# itest: no-docker"
# marker line. Opt-in (not inference) so a test that quietly grows a docker
# dependency fails loudly in review rather than silently joining this set.
if [ -n "${FLEET_ITEST_NO_DOCKER:-}" ]; then
  nodocker=()
  for test_file in "${tests[@]}"; do
    if grep -q "^# itest: no-docker" "${test_file}"; then
      nodocker+=("${test_file}")
    fi
  done
  # Length check BEFORE expanding: in bash < 4.4 (macOS ships 3.2),
  # "${nodocker[@]}" on an empty array is an unbound-variable error under
  # set -u — the graceful exit below would never be reached.
  say "No-docker subset: running ${#nodocker[@]} test(s)"
  if [ "${#nodocker[@]}" -eq 0 ]; then
    say "No no-docker tests in shard — nothing to do"
    exit 0
  fi
  tests=("${nodocker[@]}")
fi

# Kubernetes parity of the selected tests, from their "# itest:" markers (see
# common.sh), for the summary: how many run on kubernetes, how many are
# exempt (devcontainer-only, with the reasons), how many are not ported yet.
parity_k8s=0
parity_exempt=0
parity_pending=0
parity_host=0
exemptions=()
for test_file in "${tests[@]}"; do
  case "$(itest_k8s_class "${test_file}")" in
    k8s) parity_k8s=$((parity_k8s + 1)) ;;
    devcontainer-only)
      parity_exempt=$((parity_exempt + 1))
      exemptions+=("$(basename "${test_file}" .sh)|$(itest_exemption "${test_file}")")
      ;;
    host-only) parity_host=$((parity_host + 1)) ;;
    *) parity_pending=$((parity_pending + 1)) ;;
  esac
done
parity_total="${#tests[@]}"

# FLEET_ITEST_REQUIRE_K8S_PARITY=1 — the merge gate's parity check (the
# integration-k8s workflow sets it): a selection holding a devcontainer-only
# exemption or a test not ported to kubernetes fails before any test runs,
# naming each, so the count cannot grow back from 0.
if [ -n "${FLEET_ITEST_REQUIRE_K8S_PARITY:-}" ]; then
  parity_gaps=$(itest_parity_gaps "${tests[@]}")
  if [ -n "${parity_gaps}" ]; then
    printf "%b[fatal]%b k8s parity required (FLEET_ITEST_REQUIRE_K8S_PARITY): %d devcontainer-only, %d not ported yet:\n" \
      "${RED}" "${RESET}" "${parity_exempt}" "${parity_pending}" >&2
    printf '%s\n' "${parity_gaps}" | sed 's/^/    /' >&2
    printf "    mark each '# itest: k8s' once it runs on a cluster, or '# itest: no-docker' when it never makes an instance\n" >&2
    exit 1
  fi
  say "k8s parity required: ${parity_k8s} marked k8s, ${parity_host} host-only, none exempt or unported"
fi

# On kubernetes, run only the "# itest: k8s" tests. devcontainer-only ones stay
# in the list to be reported as skipped with their reason, and so do tests with
# a malformed marker, which fail below; the rest are left out like other
# shards' tests.
if [ "${FLEET_ITEST_BACKEND}" = "kubernetes" ]; then
  k8s_tests=()
  for test_file in "${tests[@]}"; do
    if [ -n "$(itest_marker_problem "${test_file}")" ]; then
      k8s_tests+=("${test_file}")
      continue
    fi
    case "$(itest_k8s_class "${test_file}")" in
      k8s|devcontainer-only) k8s_tests+=("${test_file}") ;;
    esac
  done
  # Same bash-3.2 constraint as above: length check before expanding.
  say "Kubernetes subset: ${#k8s_tests[@]} test(s): marked k8s, or to report as devcontainer-only or malformed"
  if [ "${#k8s_tests[@]}" -eq 0 ]; then
    say "No kubernetes tests in shard — nothing to do"
    exit 0
  fi
  tests=("${k8s_tests[@]}")
fi

# Shared results file — tests append one row each via common.sh helpers.
# run.sh does NOT judge results; it only aggregates this file at the end.
FLEET_ITEST_RESULTS="$(mktemp)"
export FLEET_ITEST_RESULTS

# The TUI auto-registers the fleet MCP server in ~/.claude.json, so EVERY test
# that spawns the TUI mutates the runner's real config (381 is just the one
# that asserts on it). Snapshot it once and restore on suite exit, so a dev
# running the suite locally gets their config back exactly as it was.
CLAUDE_JSON="${HOME}/.claude.json"
CLAUDE_JSON_SUITE_BAK=""
if [ -f "${CLAUDE_JSON}" ]; then
  CLAUDE_JSON_SUITE_BAK="$(mktemp)"
  cp "${CLAUDE_JSON}" "${CLAUDE_JSON_SUITE_BAK}"
fi
_restore_claude_json() {
  if [ -n "${CLAUDE_JSON_SUITE_BAK}" ] && [ -f "${CLAUDE_JSON_SUITE_BAK}" ]; then
    mv "${CLAUDE_JSON_SUITE_BAK}" "${CLAUDE_JSON}"
  else
    rm -f "${CLAUDE_JSON}"
  fi
}

trap 'rm -f "${FLEET_ITEST_RESULTS}"; _restore_claude_json' EXIT

# FLEET_ITEST_RETRIES=<n> — run a failed test again, up to n more times (CI
# sets 1: a download or runner blip fails one attempt). A test that passes
# only on a retry passes, and the summary names it as flaky.
itest_retries="${FLEET_ITEST_RETRIES:-0}"
case "${itest_retries}" in
  '' | *[!0-9]*) itest_retries=0 ;;
esac
flaky_tests=()

# FLEET_ITEST_SKIP — comma-separated list of test basenames to skip
# (without the .sh suffix). Skipped tests emit a "skip" row and never
# run; the suite does not fail on them. Used by the WSL CI job to
# exempt environment-specific known-bad tests.
#
# Held as a comma-delimited string probed with a pattern match (not an
# associative array): macOS ships bash 3.2, which has no `declare -A`,
# and the no-docker mode runs this script there.
skip_csv=""
if [ -n "${FLEET_ITEST_SKIP:-}" ]; then
  IFS=',' read -ra _skip_list <<< "${FLEET_ITEST_SKIP}"
  for s in "${_skip_list[@]}"; do
    s_trimmed=$(printf '%s' "${s}" | tr -d '[:space:]')
    [ -n "${s_trimmed}" ] && skip_csv="${skip_csv},${s_trimmed}"
  done
  skip_csv="${skip_csv},"
fi

for test_file in "${tests[@]}"; do
  test_name=$(basename "${test_file}" .sh)
  printf "\n"
  if [ -n "${skip_csv}" ] && [[ "${skip_csv}" == *",${test_name},"* ]]; then
    say "${YELLOW}SKIP${RESET} ${test_name} (FLEET_ITEST_SKIP)"
    desc=$(grep -m1 -E '^# Description:' "${test_file}" 2>/dev/null \
      | sed -E 's/^# Description: *//')
    [ -z "${desc}" ] && desc="(no description)"
    printf '%s|skip|0|%s\n' "${test_name}" "${desc}" \
      >> "${FLEET_ITEST_RESULTS}"
    continue
  fi
  marker_problem=$(itest_marker_problem "${test_file}")
  if [ -n "${marker_problem}" ]; then
    say "${RED}FAIL${RESET} ${test_name} (malformed itest marker: ${marker_problem})"
    printf '%s|fail|0|malformed itest marker: %s\n' "${test_name}" "${marker_problem}" \
      >> "${FLEET_ITEST_RESULTS}"
    continue
  fi
  if [ "${FLEET_ITEST_BACKEND}" = "kubernetes" ] \
    && [ "$(itest_k8s_class "${test_file}")" = "devcontainer-only" ]; then
    reason=$(itest_exemption "${test_file}")
    say "${YELLOW}SKIP${RESET} ${test_name} (devcontainer-only: ${reason})"
    printf '%s|skip|0|devcontainer-only: %s\n' "${test_name}" "${reason}" \
      >> "${FLEET_ITEST_RESULTS}"
    continue
  fi
  attempt=0
  while :; do
    if [ "${attempt}" -eq 0 ]; then
      say "${BOLD}RUN${RESET}  ${test_name}"
    else
      say "${YELLOW}RETRY${RESET} ${test_name} (attempt $((attempt + 1)) of $((itest_retries + 1)))"
    fi
    rc=0
    bash "${test_file}" || rc=$?
    # FLEET_ITEST_ARTIFACTS=<dir>: a failed test leaves its daemon's log and
    # the backend's state (_be_dump) there, before the teardown wipes them.
    if [ "${rc}" -ne 0 ] && [ -n "${FLEET_ITEST_ARTIFACTS:-}" ]; then
      artifacts="${FLEET_ITEST_ARTIFACTS}/${test_name}"
      [ "${attempt}" -gt 0 ] && artifacts="${artifacts}.retry${attempt}"
      mkdir -p "${artifacts}"
      cp "${HOME}/.fleet/fleet.log" "${artifacts}/fleet.log" 2>/dev/null || true
      cp "${HOME}/.fleet/state.json" "${artifacts}/state.json" 2>/dev/null || true
      _be_dump "${artifacts}" || true
      say "artifacts of ${test_name}: ${artifacts}"
    fi
    say "teardown ${test_name}"
    teardown_test || true
    if [ "${rc}" -eq 0 ] || [ "${attempt}" -ge "${itest_retries}" ]; then
      break
    fi
    # The retry writes its own row: drop the failed attempt's.
    grep -v "^${test_name}|" "${FLEET_ITEST_RESULTS}" > "${FLEET_ITEST_RESULTS}.retry" || true
    mv "${FLEET_ITEST_RESULTS}.retry" "${FLEET_ITEST_RESULTS}"
    attempt=$((attempt + 1))
  done
  if [ "${rc}" -eq 0 ] && [ "${attempt}" -gt 0 ]; then
    flaky_tests+=("${test_name}")
  fi
done

# Aggregate the per-test rows.
passed=0
failed=0
skipped=0
failed_tests=()
result_rows=()
if [ -s "${FLEET_ITEST_RESULTS}" ]; then
  while IFS='|' read -r name status dur desc; do
    [ -z "${name}" ] && continue
    result_rows+=("${name}|${status}|${dur}|${desc}")
    case "${status}" in
      pass) passed=$((passed + 1)) ;;
      skip) skipped=$((skipped + 1)) ;;
      *)
        failed=$((failed + 1))
        failed_tests+=("${name}")
        ;;
    esac
  done < "${FLEET_ITEST_RESULTS}"
fi

# Any test file that did not emit a row at all is an unexplained crash
# (e.g. syntax error before itest_begin ran). Surface it as a failure.
# Same bash-3.2 constraint as skip_csv above: no associative arrays.
seen_csv=","
for row in "${result_rows[@]}"; do
  IFS='|' read -r n _ _ _ <<< "${row}"
  seen_csv="${seen_csv}${n},"
done
for test_file in "${tests[@]}"; do
  n=$(basename "${test_file}" .sh)
  if [[ "${seen_csv}" != *",${n},"* ]]; then
    failed=$((failed + 1))
    failed_tests+=("${n}")
    result_rows+=("${n}|fail|0|(test did not emit a result — crashed before itest_begin?)")
  fi
done

if [ "${skipped}" -gt 0 ]; then
  printf "\n%b==>%b Summary: %b%d passed%b, %b%d failed%b, %b%d skipped%b\n" \
    "${BOLD}" "${RESET}" \
    "${GREEN}"  "${passed}"  "${RESET}" \
    "${RED}"    "${failed}"  "${RESET}" \
    "${YELLOW}" "${skipped}" "${RESET}"
else
  printf "\n%b==>%b Summary: %b%d passed%b, %b%d failed%b\n" \
    "${BOLD}" "${RESET}" \
    "${GREEN}" "${passed}" "${RESET}" \
    "${RED}"   "${failed}" "${RESET}"
fi

if [ "${#flaky_tests[@]}" -gt 0 ]; then
  printf "%b==>%b %bFlaky%b (passed on a retry): %s\n" "${BOLD}" "${RESET}" "${YELLOW}" "${RESET}" "${flaky_tests[*]}"
fi

# Kubernetes parity of the selected tests (counted before the kubernetes run's
# own selection), then each devcontainer-only exemption with its reason.
parity_line="k8s parity of ${parity_total} test(s): ${parity_k8s} marked k8s, ${parity_exempt} devcontainer-only, ${parity_pending} not ported yet, ${parity_host} host-only"
printf "%b==>%b Backend %s; %s\n" "${BOLD}" "${RESET}" "${FLEET_ITEST_BACKEND}" "${parity_line}"
if [ "${#exemptions[@]}" -gt 0 ]; then
  for row in "${exemptions[@]}"; do
    IFS='|' read -r name reason <<< "${row}"
    printf "    devcontainer-only %s: %s\n" "${name}" "${reason}"
  done
fi

# Emit a GitHub Actions step summary if running inside a workflow.
# GITHUB_STEP_SUMMARY is a file path the runner publishes as markdown on
# the job summary page. Outside Actions this env var is unset and we skip.
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  total=$((passed + failed + skipped))
  if [ "${failed}" -eq 0 ]; then
    if [ "${skipped}" -gt 0 ]; then
      overall="✅ all green (with ${skipped} skipped)"
    else
      overall="✅ all green"
    fi
  else
    overall="❌ ${failed} failing"
  fi
  {
    printf '## Integration Tests — %s\n\n' "${overall}"
    printf '**%d passed**, **%d failed**, **%d skipped** out of %d.\n\n' \
      "${passed}" "${failed}" "${skipped}" "${total}"
    printf 'Backend `%s`; %s.\n\n' "${FLEET_ITEST_BACKEND}" "${parity_line}"
    printf '| Test | Status | Duration | Description |\n'
    printf '| --- | --- | ---: | --- |\n'
    for row in "${result_rows[@]}"; do
      IFS='|' read -r name status dur desc <<< "${row}"
      case "${status}" in
        pass) icon="✅ pass" ;;
        skip) icon="⏭️ skip" ;;
        *)    icon="❌ fail" ;;
      esac
      printf '| `%s` | %s | %ss | %s |\n' "${name}" "${icon}" "${dur}" "${desc}"
    done
    if [ "${failed}" -gt 0 ]; then
      printf '\n**Failed:** %s\n' "${failed_tests[*]}"
    fi
    if [ "${#flaky_tests[@]}" -gt 0 ]; then
      printf '\n⚠️ **Flaky** (failed, then passed on a retry): %s\n' "${flaky_tests[*]}"
    fi
    if [ "${#exemptions[@]}" -gt 0 ]; then
      printf '\n<details><summary>devcontainer-only exemptions (%d)</summary>\n\n' "${#exemptions[@]}"
      for row in "${exemptions[@]}"; do
        IFS='|' read -r name reason <<< "${row}"
        printf -- '- `%s`: %s\n' "${name}" "${reason}"
      done
      printf '\n</details>\n'
    fi
  } >> "${GITHUB_STEP_SUMMARY}"
fi

if [ "${failed}" -gt 0 ]; then
  printf "Failed: %s\n" "${failed_tests[*]}"
  exit 1
fi
