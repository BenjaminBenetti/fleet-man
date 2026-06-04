#!/usr/bin/env bash
# Description: concurrent `fleet up` of the SAME name — exactly one wins, the rest fail cleanly, no corruption.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true; }
itest_begin

setup_test

# Several `fleet up alpha` at once race the server-side pre-create. state.Update
# serializes it: the first creates the StatusCreating record + provisions; the
# rest must see it already exists and fail fast with AlreadyExists. The end state
# must have exactly ONE alpha (no double-create, no torn state.json).
N=5
tmpdir=$(mktemp -d)
info "launching ${N} concurrent 'fleet up alpha'"
pids=""
for i in $(seq 1 "${N}"); do
  # Capture the rc INSIDE the subshell with `|| rc=$?` — a bare `cmd; echo $?`
  # would abort the subshell under `set -e` on the (expected) loser failures
  # before the rc file is ever written.
  (
    rc=0
    "${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" >"${tmpdir}/out.${i}" 2>&1 || rc=$?
    echo "${rc}" >"${tmpdir}/rc.${i}"
  ) &
  pids="${pids} $!"
done
for p in ${pids}; do wait "${p}" || true; done

ok=0; failed=0
for i in $(seq 1 "${N}"); do
  rc=$(cat "${tmpdir}/rc.${i}" 2>/dev/null || echo 99)
  if [ "${rc}" -eq 0 ]; then ok=$((ok + 1)); else failed=$((failed + 1)); fi
done
info "successes=${ok} failures=$((N - ok))"
assert_equals "1" "${ok}" "exactly one concurrent 'fleet up alpha' should succeed"

# The losers must fail with a clean AlreadyExists, not a panic / torn write.
fail_out=$(cat "${tmpdir}"/out.* 2>/dev/null || true)
assert_contains "${fail_out}" "already exists" "concurrent duplicate should report AlreadyExists cleanly"

# state.json valid + exactly one alpha record.
assert_file_exists "${HOME}/.fleet/state.json"
if command -v python3 >/dev/null 2>&1; then
  python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "${HOME}/.fleet/state.json" \
    || fail "state.json is not valid JSON — a concurrent write tore it"
fi
state=$(cat "${HOME}/.fleet/state.json")
n=$(printf '%s' "${state}" | grep -oE '"name": *"alpha"' | grep -c . || true)
assert_equals "1" "${n}" "state.json must contain exactly one 'alpha' (no concurrent double-create)"

rm -rf "${tmpdir}"
pass "concurrent up (same name) — one winner, clean failures, no corruption"
