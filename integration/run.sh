#!/usr/bin/env bash
# Integration test driver for fleet-man.
#
# - Builds the fleet binary to a temp location (unless FLEET_BIN is set).
# - Runs every test in integration/tests in alphabetical order.
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

# 1. Build fleet binary unless caller provided one.
if [ -z "${FLEET_BIN:-}" ]; then
  build_dir="$(mktemp -d)"
  FLEET_BIN="${build_dir}/fleet"
  say "Building fleet -> ${FLEET_BIN}"
  (cd "${REPO_ROOT}" && go build -o "${FLEET_BIN}" ./cmd/fleet)
fi
export FLEET_BIN
say "Using fleet binary: ${FLEET_BIN}"
"${FLEET_BIN}" --help >/dev/null || { printf "%b[fatal]%b fleet binary failed to run\n" "${RED}" "${RESET}" >&2; exit 2; }

# 2. Preflight: docker + devcontainer CLI must be available and usable.
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

# 3. Pre-pull the fixture image once so each test is not waiting on image pulls.
fixture_image=$(grep -oE '"image":\s*"[^"]+"' "${INTEGRATION_DIR}/fixture/.devcontainer/devcontainer.json" | sed -E 's/.*"([^"]+)"$/\1/')
if [ -n "${fixture_image}" ]; then
  say "Pre-pulling fixture image: ${fixture_image}"
  docker pull -q "${fixture_image}" >/dev/null || true
fi

# 4. Iterate tests.
shopt -s nullglob
tests=("${INTEGRATION_DIR}"/tests/*.sh)
shopt -u nullglob
if [ "${#tests[@]}" -eq 0 ]; then
  printf "%b[fatal]%b no tests found in %s\n" "${RED}" "${RESET}" "${INTEGRATION_DIR}/tests" >&2
  exit 2
fi

# Shared results file — tests append one row each via common.sh helpers.
# run.sh does NOT judge results; it only aggregates this file at the end.
FLEET_ITEST_RESULTS="$(mktemp)"
export FLEET_ITEST_RESULTS
trap 'rm -f "${FLEET_ITEST_RESULTS}"' EXIT

# Sourced common.sh in tests provides teardown_test — we need it here too.
# shellcheck disable=SC1091
source "${INTEGRATION_DIR}/common.sh"

# FLEET_ITEST_SKIP — comma-separated list of test basenames to skip
# (without the .sh suffix). Skipped tests emit a "skip" row and never
# run; the suite does not fail on them. Used by the WSL CI job to
# exempt environment-specific known-bad tests.
declare -A skip_set=()
if [ -n "${FLEET_ITEST_SKIP:-}" ]; then
  IFS=',' read -ra _skip_list <<< "${FLEET_ITEST_SKIP}"
  for s in "${_skip_list[@]}"; do
    s_trimmed=$(printf '%s' "${s}" | tr -d '[:space:]')
    [ -n "${s_trimmed}" ] && skip_set["${s_trimmed}"]=1
  done
fi

for test_file in "${tests[@]}"; do
  test_name=$(basename "${test_file}" .sh)
  printf "\n"
  if [ -n "${skip_set[${test_name}]:-}" ]; then
    say "${YELLOW}SKIP${RESET} ${test_name} (FLEET_ITEST_SKIP)"
    desc=$(grep -m1 -E '^# Description:' "${test_file}" 2>/dev/null \
      | sed -E 's/^# Description: *//')
    [ -z "${desc}" ] && desc="(no description)"
    printf '%s|skip|0|%s\n' "${test_name}" "${desc}" \
      >> "${FLEET_ITEST_RESULTS}"
    continue
  fi
  say "${BOLD}RUN${RESET}  ${test_name}"
  bash "${test_file}" || true
  say "teardown ${test_name}"
  teardown_test || true
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
declare -A seen_names
for row in "${result_rows[@]}"; do
  IFS='|' read -r n _ _ _ <<< "${row}"
  seen_names["${n}"]=1
done
for test_file in "${tests[@]}"; do
  n=$(basename "${test_file}" .sh)
  if [ -z "${seen_names[${n}]:-}" ]; then
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
  } >> "${GITHUB_STEP_SUMMARY}"
fi

if [ "${failed}" -gt 0 ]; then
  printf "Failed: %s\n" "${failed_tests[*]}"
  exit 1
fi
