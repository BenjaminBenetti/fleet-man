#!/usr/bin/env bash
# Description: concurrent `fleet up` of distinct instances all land — no lost state writes (#63).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true; }
itest_begin

setup_test

# This is the literal #63 regression guard. Pre-migration each `fleet up`
# process wrote ~/.fleet/state.json directly, so concurrent ups could clobber
# one another's writes. With the fleetd single-writer (every mutation through
# state.Update) that is structurally impossible — every concurrent up must land.
#
# Warm the devcontainer image first with a throwaway instance, then drop it, so
# the concurrent ups reuse the cached image and we isolate the STATE-write race
# (not docker image-build contention).
info "warm the devcontainer image"
fleet_up warmup >/dev/null
"${FLEET_BIN}" down "${FIXTURE_REPO_NAME}/warmup" >/dev/null

# Distinct multi-char names: single-char needles would match unrelated cells in
# the `fleet ls` table (e.g. 'a' inside the 'main' branch / date columns).
names="alpha bravo charlie"
info "launching concurrent 'fleet up' for: ${names}"
pids=""
for n in ${names}; do
  fleet_up "${n}" >/dev/null 2>&1 &
  pids="${pids} $!"
done

rc=0
for p in ${pids}; do
  if ! wait "${p}"; then rc=1; fi
done
[ "${rc}" -eq 0 ] || fail "one or more concurrent 'fleet up' invocations failed"

info "fleet ls"
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
printf '%s\n' "${ls_out}"
for n in ${names}; do
  assert_contains "${ls_out}" "${n}" "instance '${n}' missing from ls — a concurrent write was lost (#63)"
done

# state.json must be valid JSON and hold exactly the three instances.
assert_file_exists "${HOME}/.fleet/state.json"
if command -v python3 >/dev/null 2>&1; then
  python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "${HOME}/.fleet/state.json" \
    || fail "state.json is not valid JSON — a concurrent write tore it (#63)"
fi
state=$(cat "${HOME}/.fleet/state.json")
count=$(printf '%s' "${state}" | grep -oE '"name": *"(alpha|bravo|charlie)"' | sort -u | grep -c . || true)
assert_equals "3" "${count}" "expected 3 distinct instances persisted, found ${count} (lost write = #63 regression)"

pass "concurrent up (distinct) — no lost writes"
