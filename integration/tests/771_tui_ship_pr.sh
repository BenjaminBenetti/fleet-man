#!/usr/bin/env bash
# itest: k8s
# Description: Ship a PR — with Claude Code enabled for the fleet, selecting the PR status (l) and pressing s starts a "ship" session tagged with the PR (@fleet_ship) that boots `claude '/ship <PR URL>'`; the row wears the ship icon, and its colours change once the daemon's probe sees the PR merged.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test

pr_url="https://github.com/example/itest-fleet/pull/4242"

# gh stub (as in 770) answering the branch probe with one open PR, plus the ship
# lookup by URL, whose state comes from /tmp/ship-pr-state (OPEN until the test
# "merges" it). Real gh applies the probe's --jq, which tags that object with
# "ship": true; the stub emits the tagged object directly.
stub_dir="${FIXTURE_REPO_DIR}/.itest-bin"
mkdir -p "${stub_dir}"
cat > "${stub_dir}/gh" <<'STUB'
#!/bin/sh
printf '%s|%s\n' "$(pwd)" "$*" >> /tmp/gh-stub.log
case "$1 $2" in
  "auth status") exit 0 ;;
  "repo view") echo main; exit 0 ;;
  "pr list") echo "OPEN 4242"; exit 0 ;;
  "pr view")
    case "$*" in *"ship: true"*)
      state=$(cat /tmp/ship-pr-state 2>/dev/null || echo OPEN)
      printf '{"ship":true,"number":4242,"state":"%s","url":"%s","title":"itest ship PR"}\n' "${state}" "$3"
      exit 0 ;;
    esac
    cat <<'JSON'
{"number":4242,"state":"OPEN","mergeStateStatus":"BLOCKED","reviewDecision":"APPROVED","url":"https://github.com/example/itest-fleet/pull/4242","title":"itest ship PR","statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"},{"state":"SUCCESS"},{"status":"IN_PROGRESS","conclusion":""}]}
JSON
    exit 0 ;;
esac
exit 1
STUB
chmod +x "${stub_dir}/gh"

# claude stub: a /ship prompt is recorded and the stub stays up like an agent
# would; anything else (the Claude Code startup script's `claude --version`)
# answers at once, so provisioning never blocks on it. On PATH for every exec
# (remoteEnv), the startup script finds it and skips the real install; once the
# instance is up it is also copied to ~/.local/bin, which the ship session's
# login shell puts on PATH (its profile may reset the exec PATH).
cat > "${stub_dir}/claude" <<'STUB'
#!/bin/sh
case "$1" in
  /ship*) printf '%s\n' "$*" > /tmp/ship-claude-args; exec sleep 600 ;;
esac
echo "0.0.0 (Claude Code stub)"
STUB
chmod +x "${stub_dir}/claude"

cat > "${FIXTURE_REPO_DIR}/.devcontainer/devcontainer.json" <<'JSON'
{
  "name": "fleet-man integration fixture (ship stubs)",
  "image": "mcr.microsoft.com/devcontainers/base:debian-12",
  "postCreateCommand": "sudo apt-get update -qq && sudo apt-get install -y -qq --no-install-recommends tmux",
  "remoteEnv": { "PATH": "${containerWorkspaceFolder}/.itest-bin:${containerEnv:PATH}" }
}
JSON
# The image has no tmux. A devcontainer installs it after create; a pod runs
# the tmux fleet bundles, so it needs no package mirror.
if itest_backend_is kubernetes; then
  dc_json="${FIXTURE_REPO_DIR}/.devcontainer/devcontainer.json"
  grep -v '"postCreateCommand"' "${dc_json}" > "${dc_json}.tmp"
  mv "${dc_json}.tmp" "${dc_json}"
fi
( cd "${FIXTURE_REPO_DIR}" && git add -A && git commit -q -m "fixture: gh + claude stubs" )

# Shipping needs the Claude Code agent enabled for the fleet.
seed_fleet_settings "${FIXTURE_REPO_NAME}" true false /home/vscode

fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

"${FLEET_BIN}" exec "${inst}" -- sh -c 'mkdir -p "$HOME/.local/bin" && cp .itest-bin/claude "$HOME/.local/bin/claude"' \
  || fail "could not put the claude stub in ~/.local/bin"
"${FLEET_BIN}" exec "${inst}" -- git -c safe.directory='*' checkout -q -b feature/ship \
  || fail "could not switch the workspace to a feature branch"
"${FLEET_BIN}" exec "${inst}" -- rm -f /tmp/gh-stub.log /tmp/ship-claude-args /tmp/ship-pr-state

info "spawning the TUI (the PR probe runs only while a TUI is connected)"
# termenv draws no colour at all while CI is set (GitHub Actions sets it), and
# this test tells shipping from shipped by the ship row's colours, so this TUI
# runs with CI unset; otherwise it is tui_spawn.
tmux kill-session -t "${TUI_SESSION}" >/dev/null 2>&1 || true
tmux new-session -d -s "${TUI_SESSION}" -x "${TUI_WIDTH}" -y "${TUI_HEIGHT}" \
  "env -u CI TERM=xterm-256color ${FLEET_BIN}"
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

tui_send j
sleep 0.5
tui_send Space
tui_wait_for "+ new session" 15
tui_wait_for "Checks 2/3" 60 || fail "the instance row never showed the PR status"

info "selecting the PR status and shipping it"
tui_send j
sleep 0.5
tui_send l
tui_wait_for "s: ship" 10 || fail "the help bar should offer s: ship on a selected PR status"
tui_send s

tui_wait_for "ship 🚢" 20 || fail "the ship session row never appeared with the ship icon"

sessions=$("${FLEET_BIN}" exec "${inst}" -- tmux list-sessions -F '#{session_name} #{@fleet_ship}') \
  || fail "could not list the instance's tmux sessions"
assert_contains "${sessions}" "alpha~ship ${pr_url}" "the ship session should carry the PR in @fleet_ship"

deadline=$(( $(date +%s) + $(_scale_timeout 20) ))
args=""
while [ "$(date +%s)" -lt "${deadline}" ]; do
  args=$("${FLEET_BIN}" exec "${inst}" -- cat /tmp/ship-claude-args 2>/dev/null || true)
  [ -n "${args}" ] && break
  sleep 0.5
done
[ "${args}" = "/ship ${pr_url}" ] \
  || fail "claude should start with /ship <PR URL> as its prompt, got: ${args:-<never started>}"

# Park the cursor off the ship row so it renders in its own colours, not the
# selection style.
tui_send h
tui_send k
sleep 0.5
ship_line() { tmux capture-pane -t "${TUI_SESSION}" -p -e | grep -F "🚢" | head -1; }
before=$(ship_line)
[ -n "${before}" ] || fail "no ship row to inspect"

info "merging the PR and waiting for the probe to turn the ship session green"
"${FLEET_BIN}" exec "${inst}" -- sh -c 'echo MERGED > /tmp/ship-pr-state'
deadline=$(( $(date +%s) + $(_scale_timeout 150) ))
after="${before}"
while [ "$(date +%s)" -lt "${deadline}" ]; do
  after=$(ship_line)
  [ -n "${after}" ] && [ "${after}" != "${before}" ] && break
  sleep 1
done
if [ "${after}" = "${before}" ]; then
  printf -- '--- gh stub log ---\n%s\n--- end ---\n' \
    "$("${FLEET_BIN}" exec "${inst}" -- cat /tmp/gh-stub.log 2>&1 || true)" >&2
  fail "the ship row's colours never changed after the PR merged"
fi
assert_contains "$(printf '%s' "${after}" | sed 's/\x1b\[[0-9;]*m//g')" "ship 🚢" "the merged ship row should keep its name and icon"

log=$("${FLEET_BIN}" exec "${inst}" -- cat /tmp/gh-stub.log) || fail "the gh stub never ran"
assert_contains "${log}" "pr view ${pr_url} --json" "the probe should look the ship's PR up by URL"

pass "s on the PR status ships it: a tagged ship session runs claude /ship, and turns green once merged"
