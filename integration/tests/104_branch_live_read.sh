#!/usr/bin/env bash
# Description: A branch switched inside an instance shows in `fleet ls`, MCP fleet_list and the TUI (read by the daemon), and still does after a stop and a daemon restart.
# itest: k8s
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test

inst="${FIXTURE_REPO_NAME}/alpha"

# ls_branch prints alpha's BRANCH column (FLEET INSTANCE STATUS CONTAINER
# CREATED-date CREATED-time BRANCH), empty when the column is blank.
ls_branch() {
  "${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" | awk '$2 == "alpha" { print $7 }'
}

# mcp_current_branch prints alpha's current_branch from the daemon's MCP
# fleet_list tool (the fleet holds one instance).
mcp_current_branch() {
  itest_mcp_call fleet_list "{\"fleet\":\"${FIXTURE_REPO_NAME}\"}" structuredContent.instances.0.current_branch
}

# A host checkout read (devcontainer) is repeated on every call, so a switch
# must show at once. A read that goes into the instance may be reused by the
# daemon for up to 10s (GetState's branch cache), so other backends get a
# grace period.
branch_grace=15
if itest_backend_is devcontainer; then
  branch_grace=0
fi

# assert_branch <want> <context>: fleet ls and MCP fleet_list agree on <want>.
assert_branch() {
  local want="$1" ctx="$2" got deadline
  deadline=$(( $(date +%s) + branch_grace ))
  while :; do
    got="$(ls_branch)"
    { [ "${got}" = "${want}" ] || [ "$(date +%s)" -ge "${deadline}" ]; } && break
    sleep 1
  done
  "${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}"
  assert_equals "${want}" "${got}" "fleet ls BRANCH ${ctx}"
  while :; do
    got="$(mcp_current_branch)" || fail "MCP fleet_list did not answer ${ctx}"
    { [ "${got}" = "${want}" ] || [ "$(date +%s)" -ge "${deadline}" ]; } && break
    sleep 1
  done
  assert_equals "${want}" "${got}" "fleet_list current_branch ${ctx}"
}

info "fleet up alpha (fixture on main)"
fleet_up alpha
assert_branch "main" "after create"

# ===========================================
# Switch branches INSIDE the instance: the daemon reads it from the
# workspace, and persists it as the instance's last_branch.
# ===========================================
info "git checkout -b feature/live inside the instance"
"${FLEET_BIN}" exec "${inst}" -- git -c safe.directory='*' checkout -q -b feature/live
assert_branch "feature/live" "after an in-instance switch"
grep -qE '"last_branch": *"feature/live"' "${HOME}/.fleet/state.json" \
  || fail "state.json should record last_branch feature/live: $(cat "${HOME}/.fleet/state.json")"

info "fleet stop alpha"
"${FLEET_BIN}" stop "${inst}"
assert_branch "feature/live" "while stopped"

expected="feature/live"
if itest_backend_is devcontainer; then
  # The devcontainer workspace IS the host checkout, and its reader keeps
  # reading it while the instance is stopped: a switch made there now shows
  # (rather than the last branch seen while running). Other backends have no
  # host checkout to switch, so they keep the last branch seen.
  ws_dir=$(itest_host_workspace_dir "${inst}")
  info "switch the host checkout while stopped"
  git -C "${ws_dir}" checkout -q -b feature/while-stopped
  expected="feature/while-stopped"
  assert_branch "${expected}" "after a host switch while stopped"
fi

# ===========================================
# A restarted daemon has no live runtime yet; the branch must not blank.
# ===========================================
info "restart the daemon"
pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
for _ in $(seq 1 40); do
  [ "$(server_count)" -eq 0 ] && break
  sleep 0.25
done
[ "$(server_count)" -eq 0 ] || fail "daemon did not exit"
assert_branch "${expected}" "after a daemon restart"

info "TUI shows the branch on the stopped row"
tui_spawn
tui_wait_for "${expected} ⬡" 20 || fail "TUI row should show ${expected}"

info "fleet start alpha, then switch inside the instance with the TUI open"
"${FLEET_BIN}" start "${inst}"
"${FLEET_BIN}" exec "${inst}" -- git -c safe.directory='*' checkout -q -b feature/after-restart
tui_wait_for "feature/after-restart ⬡" 20 || fail "TUI row should follow the in-instance switch"
assert_branch "feature/after-restart" "after start and another switch"

pass "branch switched inside the instance is reported by ls, fleet_list and the TUI across stop and daemon restart"
