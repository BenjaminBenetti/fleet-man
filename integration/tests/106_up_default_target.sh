#!/usr/bin/env bash
# Description: `fleet up` without --backend uses fleetd's remembered deploy target; the TUI New instance dialog opens on it and remembers the target it creates on.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test

config="${HOME}/.fleet/config.json"
state="${HOME}/.fleet/state.json"

# remember_target <backend> records a remembered default deploy target, as a
# TUI create earlier left it. fleetd reads config.json on every create.
remember_target() {
  if [ -f "${config}" ]; then
    jq --arg b "$1" '.default_backend = $b' "${config}" > "${config}.tmp"
    mv "${config}.tmp" "${config}"
  else
    printf '{"default_backend": "%s"}\n' "$1" > "${config}"
  fi
}

remembered_target() {
  [ -f "${config}" ] || { echo ""; return; }
  jq -r '.default_backend // ""' "${config}"
}

recorded_backend() {
  jq -r --arg f "${FIXTURE_REPO_NAME}" --arg i "$1" \
    '.fleets[$f].instances[] | select(.name == $i) | .backend' "${state}"
}

# ===========================================
# Nothing remembered: an unspecified create goes to devcontainer.
# ===========================================
info "fleet up alpha without --backend, nothing remembered"
out=$("${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" 2>&1) || fail "fleet up alpha failed: ${out}"
printf '%s\n' "${out}"
assert_contains "${out}" "Creating ${FIXTURE_REPO_NAME}/alpha (backend: devcontainer)" "fleet up should name the target fleetd resolved"
assert_equals "devcontainer" "$(recorded_backend alpha)" "alpha should be a devcontainer instance"
assert_equals "" "$(remembered_target)" "the CLI must not remember a default target"

# ===========================================
# A remembered target is where `fleet up` without --backend goes. Nothing in
# this environment can provision a codespace, so the create fails; what
# matters is that it went to codespaces, not to a hard-coded devcontainer.
# ===========================================
remember_target codespaces
info "fleet up beta without --backend, codespaces remembered"
set +e
out=$("${FLEET_BIN}" up "${FIXTURE_REPO_NAME}/beta" 2>&1); rc=$?
set -e
printf '%s\n' "${out}"
assert_contains "${out}" "Creating ${FIXTURE_REPO_NAME}/beta (backend: codespaces)" "fleet up without --backend should use the remembered target"
assert_equals "codespaces" "$(recorded_backend beta)" "beta should be recorded on the remembered codespaces target"
[ "${rc}" -ne 0 ] || info "beta unexpectedly provisioned on codespaces"
"${FLEET_BIN}" down "${FIXTURE_REPO_NAME}/beta" >/dev/null 2>&1 || fail "could not remove the beta record"

assert_equals "codespaces" "$(remembered_target)" "a CLI create must not move the remembered target"

# ===========================================
# The TUI's New instance dialog opens on the daemon's remembered target, and
# the target it creates on becomes the remembered one (server-side, not a
# client config save).
# ===========================================
tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

info "pressing 'a' on the fleet header"
tui_send a
tui_wait_for "instance-name" 5
if command -v gh >/dev/null 2>&1; then
  # fleetd (started from this shell) finds gh, so Codespaces is available and
  # the dialog opens on it.
  tui_assert_contains "[ Codespaces ]" "the dialog should open on the remembered codespaces target"
  info "cycling the Deploy row to Devcontainer"
  tui_send Escape
  sleep 0.3
  tui_send Tab
  tui_wait_for "[ Devcontainer ]" 5
else
  info "no gh where fleetd runs: codespaces is unavailable, so the dialog opens on Devcontainer"
  tui_assert_contains "[ Devcontainer ]" "the dialog should fall back to an available target"
  tui_send Escape
  sleep 0.3
fi

info "naming the instance gamma and creating it"
tui_send Enter
sleep 0.3
tui_send_text "gamma"
sleep 0.3
tui_send Enter
tui_wait_for_absent "instance-name" 5

info "waiting for gamma to run"
for _ in $(seq 1 240); do
  status=$(jq -r --arg f "${FIXTURE_REPO_NAME}" \
    '.fleets[$f].instances[] | select(.name == "gamma") | .status' "${state}" 2>/dev/null || true)
  case "${status}" in
    running) break ;;
    failed) fail "gamma failed to provision" ;;
  esac
  sleep 1
done
assert_equals "running" "${status}" "gamma should be running"
assert_equals "devcontainer" "$(recorded_backend gamma)" "gamma should be on the picked devcontainer target"
assert_equals "devcontainer" "$(remembered_target)" "the TUI create should remember the target it used"

tui_kill

info "fleet up delta without --backend now goes to the newly remembered devcontainer"
out=$("${FLEET_BIN}" up "${FIXTURE_REPO_NAME}/delta" 2>&1) || fail "fleet up delta failed: ${out}"
printf '%s\n' "${out}"
assert_contains "${out}" "Creating ${FIXTURE_REPO_NAME}/delta (backend: devcontainer)" "fleet up should follow the remembered target"

pass "fleet up and the New instance dialog share fleetd's remembered deploy target"
