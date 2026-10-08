#!/usr/bin/env bash
# Description: Fleet status mod — every devcontainer instance's shells load the
# fleet-status Claude Code mod (CLAUDE_CODE_PLUGIN_DIRS → a plugin of function
# hooks the fleet binary carries), and the daemon keeps the mod's status file in
# each instance's control directory: readable inside the instance, naming it,
# live (with the agent counts) only while a TUI has the runtime polled. Turning
# the global "Fleet status mod" setting off removes the file (the mod then draws
# nothing); turning it on brings it back for the running instance.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"
status_file="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/alpha/.control/claude-status.json"

# in_instance runs a command from an interactive bash in the instance, the way
# a tmux session does (it sources ~/.bashrc, hence fleet.rc).
in_instance() { "${FLEET_BIN}" exec "${inst}" -- bash -ic "$1" 2>/dev/null || true; }

# wait_status <present|absent> waits for the daemon's next passes to follow.
wait_status() {
  local deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
  until { [ "$1" = present ] && [ -s "${status_file}" ]; } || { [ "$1" = absent ] && [ ! -e "${status_file}" ]; }; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "the status file never became $1 (${status_file})"
    sleep 0.5
  done
}

# set_status_mod <true|false> flips the global setting in config.json (the
# daemon re-reads it every pass).
set_status_mod() {
  python3 - "${HOME}/.fleet/config.json" "$1" <<'PY'
import json, os, sys
path, want = sys.argv[1], sys.argv[2] == "true"
try:
    with open(path) as f:
        cfg = json.load(f)
except FileNotFoundError:
    cfg = {}
cfg.setdefault("claude_code_settings", {})["status_mod"] = want
tmp = path + ".itest"
with open(tmp, "w") as f:
    json.dump(cfg, f, indent=2)
os.replace(tmp, path)
PY
}

info "an instance's shells load the mod"
out=$(in_instance 'echo "PLUGIN_DIRS=[${CLAUDE_CODE_PLUGIN_DIRS:-}]"; ls "${HOME}/.cache/fleet/claude-mod/fleet-status/hooks"; cat "${HOME}/.cache/fleet/claude-mod/fleet-status/.claude-plugin/plugin.json"')
printf 'shell:\n%s\n' "${out}"
assert_contains "${out}" ".cache/fleet/claude-mod/fleet-status" "the shell was not handed the mod via CLAUDE_CODE_PLUGIN_DIRS"
assert_contains "${out}" "register.tsx" "the mod's hooks module was not written"
assert_contains "${out}" '"name": "fleet-status"' "the mod's manifest was not written"

info "with no TUI the runtime is not polled: the file only names the instance"
wait_status present
host=$(cat "${status_file}")
printf 'status file: %s\n' "${host}"
assert_contains "${host}" '"instance":"alpha"' "the not-live status file does not name the instance"
assert_contains "${host}" '"live":false' "a status file claims live counts with nothing polling the agents"

info "with a TUI connected the file goes live, with the agent counts"
tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
until grep -q '"live":true' "${status_file}" 2>/dev/null; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the status file never went live: $(cat "${status_file}" 2>/dev/null)"
  sleep 0.5
done
host=$(cat "${status_file}")
printf 'status file: %s\n' "${host}"
assert_contains "${host}" "\"fleet\":\"${FIXTURE_REPO_NAME}\"" "the status file does not name the fleet"
assert_contains "${host}" '"instance":"alpha"' "the status file does not name the instance"
assert_contains "${host}" '"working":0' "the status file has no working count"
assert_contains "${host}" '"idle":0' "the status file has no idle count"

out=$(in_instance 'cat /fleet-mounts/control/claude-status.json')
assert_contains "${out}" '"instance":"alpha"' "the instance cannot read its status file: ${out}"

if [ -n "$(in_instance 'command -v claude')" ]; then
  info "the installed mod passes the instance's own Claude Code checks"
  out=$(in_instance 'claude plugin validate "${HOME}/.cache/fleet/claude-mod/fleet-status" 2>&1; echo "VALIDATE_EXIT=$?"')
  printf 'validate:\n%s\n' "${out}"
  assert_contains "${out}" "VALIDATE_EXIT=0" "claude plugin validate rejected the installed mod"
else
  info "no claude in the instance; skipping the plugin validation"
fi

info "turning the setting off removes the file"
set_status_mod false
wait_status absent

info "turning it back on restores it for the running instance"
set_status_mod true
wait_status present
assert_contains "$(cat "${status_file}")" '"instance":"alpha"' "the restored status file does not name the instance"

pass "fleet status mod: loaded in every instance, fed by the daemon while the setting is on"
