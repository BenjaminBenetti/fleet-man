#!/usr/bin/env bash
# go-deep.sh — the Go checks too long for every push: the sharesync
# property tests at SHARESYNC_SEEDS (200) seeds, and each fuzz target for
# FLEET_NIGHTLY_FUZZTIME (60s) beyond its seed corpus. No cluster. A fuzz
# failure leaves its input under the package's testdata/fuzz, which the
# job's artifact keeps.
set -euo pipefail
source "$(dirname "$0")/lib.sh"

nightly_begin go-deep
nightly_require go
fuzztime="${FLEET_NIGHTLY_FUZZTIME:-60s}"
cd "${REPO_ROOT}"

log="${NIGHTLY_OUT}/sharesync-properties.log"
if SHARESYNC_SEEDS="${SHARESYNC_SEEDS:-200}" go test -count=1 -timeout 60m -run 'Propert' -v ./internal/sharesync > "${log}" 2>&1; then
  nightly_note "- sharesync properties at ${SHARESYNC_SEEDS:-200} seeds: $(grep -c '^--- PASS' "${log}") passed"
else
  tail -n 40 "${log}" >&2
  nightly_fail "sharesync properties"
fi

# Every fuzz target in the module, by package.
grep -rl --include='*_test.go' '^func Fuzz' internal | xargs -n1 dirname | sort -u | while read -r pkg; do
  for target in $(grep -h '^func Fuzz' "${pkg}"/*_test.go | sed -E 's/^func (Fuzz[A-Za-z0-9_]+).*/\1/' | sort -u); do
    log="${NIGHTLY_OUT}/fuzz-${target}.log"
    if go test -run '^$' -fuzz "^${target}\$" -fuzztime "${fuzztime}" -fuzzminimizetime 10x "./${pkg}" > "${log}" 2>&1; then
      echo "- fuzz ${target} (${pkg}): $(grep -oE 'execs: [0-9]+' "${log}" | tail -n 1)" >> "${NIGHTLY_OUT}/fuzz.md"
    else
      tail -n 20 "${log}" >&2
      echo "- **FAILED** fuzz ${target} (${pkg})" >> "${NIGHTLY_OUT}/fuzz.md"
      echo "${target}" >> "${NIGHTLY_OUT}/fuzz-failed"
    fi
  done
done
cat "${NIGHTLY_OUT}/fuzz.md" >> "${NIGHTLY_OUT}/summary.md"
if [ -s "${NIGHTLY_OUT}/fuzz-failed" ]; then
  nightly_fail "fuzz: $(tr '\n' ' ' < "${NIGHTLY_OUT}/fuzz-failed")"
fi
nightly_end
