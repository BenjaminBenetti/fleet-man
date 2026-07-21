#!/usr/bin/env bash
# itest: no-docker
# Description: fleet agent / fleet trigger CLI CRUD (issue #189) — create, list,
# edit (rename), and delete automation agents and triggers through a real
# daemon, asserting the changes round-trip to state.json, that a rename rewrites
# trigger references, that a referenced agent can't be deleted, and that the
# read-modify-write preserves unrelated fleet settings.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
# Kill our daemon on exit so its in-memory state (our seeded fleet + triggers)
# can't bleed into a later test. Mirrors 391/611.
itest_cleanup() { pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true; }
itest_begin


setup_test

# A daemon lingers across tests (teardown wipes ~/.fleet but never kills the
# Setsid daemon), and the socket is HOME-scoped and shared. Kill any lingering
# one so the first CLI call below cold-spawns a fresh daemon that loads the
# state.json we seed.
info "killing any lingering daemon so we cold-spawn one with our seeded state"
pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
deadline=$(( $(date +%s) + $(_scale_timeout 5) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  [ "$(server_count)" = "0" ] && break
  sleep 0.2
done

FLEET="${FIXTURE_REPO_NAME}"
# Seed a bare fleet carrying a non-default setting (claudeCodeMount=true) so we
# can prove the automation writes preserve unrelated settings. claudeCodeMount
# is omitempty, so if a write ever clobbered it to false the key would vanish
# entirely — its continued presence is the proof.
seed_fleet_settings "${FLEET}" true false /home/node

# --- agents: create + list ---
info "creating an agent"
out=$("${FLEET_BIN}" agent create "${FLEET}" builder --backend devcontainer --system-prompt "be terse" 2>&1) \
  || fail "agent create failed: ${out}"
assert_contains "${out}" "Created agent" "agent create did not confirm"

out=$("${FLEET_BIN}" agent list "${FLEET}" 2>&1) || fail "agent list failed: ${out}"
assert_contains "${out}" "builder" "agent list missing the new agent"
assert_contains "${out}" "devcontainer" "agent list missing the backend"

state=$(cat "${HOME}/.fleet/state.json")
assert_contains "${state}" "builder" "agent not persisted to state.json"
assert_contains "${state}" "claudeCodeMount" "automation write clobbered claudeCodeMount"

# --- triggers: create + list ---
# A cron that won't match during CI (midnight, Jan 1) so the scheduler never
# spawns a real instance while this CLI test runs.
info "creating a trigger"
out=$("${FLEET_BIN}" trigger create "${FLEET}" nightly --agent builder --cron "0 0 1 1 *" --prompt "go" 2>&1) \
  || fail "trigger create failed: ${out}"
assert_contains "${out}" "Created trigger" "trigger create did not confirm"

out=$("${FLEET_BIN}" trigger list "${FLEET}" 2>&1) || fail "trigger list failed: ${out}"
assert_contains "${out}" "nightly" "trigger list missing the new trigger"
assert_contains "${out}" "builder" "trigger list missing the agent reference"

# The agent list's TRIGGERS column now reports the reference count for builder.
# Columns are NAME BACKEND TRIGGERS COMMAND, so match builder's row with a 1.
out=$("${FLEET_BIN}" agent list "${FLEET}" 2>&1) || fail "agent list (after trigger) failed: ${out}"
printf '%s' "${out}" | grep -qE 'builder[[:space:]]+devcontainer[[:space:]]+1' \
  || fail "agent list TRIGGERS column should show 1 for builder: ${out}"

# --- delete guard: a referenced agent can't be deleted ---
info "verifying a referenced agent cannot be deleted"
if "${FLEET_BIN}" agent delete "${FLEET}" builder >/dev/null 2>&1; then
  fail "deleting a referenced agent should have failed"
fi
assert_contains "$(cat "${HOME}/.fleet/state.json")" "builder" "agent wrongly removed despite the reference guard"

# --- edit: rename rewrites trigger refs ---
info "renaming the agent (trigger references must follow)"
out=$("${FLEET_BIN}" agent edit "${FLEET}" builder --rename builder2 2>&1) || fail "agent edit failed: ${out}"
assert_contains "$(cat "${HOME}/.fleet/state.json")" "builder2" "rename not persisted"
out=$("${FLEET_BIN}" trigger list "${FLEET}" 2>&1)
assert_contains "${out}" "builder2" "trigger reference not rewritten on rename"

# --- trigger edit (changed-only) ---
"${FLEET_BIN}" trigger edit "${FLEET}" nightly --prompt "updated" >/dev/null 2>&1 \
  || fail "trigger edit failed"

# --- delete: trigger first, then the now-unreferenced agent ---
info "deleting the trigger, then the now-free agent"
"${FLEET_BIN}" trigger delete "${FLEET}" nightly >/dev/null 2>&1 || fail "trigger delete failed"
"${FLEET_BIN}" agent delete "${FLEET}" builder2 >/dev/null 2>&1 || fail "agent delete (after detach) failed"

state=$(cat "${HOME}/.fleet/state.json")
assert_not_contains "${state}" "builder2" "agent still present after delete"
assert_not_contains "${state}" "nightly" "trigger still present after delete"
# The fleet and its unrelated settings survive the automation deletes.
assert_contains "${state}" "claudeCodeMount" "fleet settings lost after automation deletes"

pass "fleet agent/trigger CLI: create, list, edit/rename, reference guard, and delete all work"
