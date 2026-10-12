#!/usr/bin/env bash
# itest: k8s
# Description: PR status auto-tag — with a TUI connected, the daemon runs the gh probe inside the instance (cwd the workspace checkout, a gh stub on PATH answering for the checked-out feature branch) and the instance row shows the open PR's status, "PR  Accepted  Checks 2/3".
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test

# The stub answers the probe's gh calls (auth status, pr list for the branch,
# pr view of the PR) and logs each one with its working directory. It sits in
# the repo and remoteEnv puts it first on PATH for every exec, the way the
# probe's exec class sees an installed gh, without needing root in the
# instance.
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
    cat <<'JSON'
{"number":4242,"state":"OPEN","mergeStateStatus":"BLOCKED","reviewDecision":"APPROVED","url":"https://github.com/example/itest-fleet/pull/4242","title":"itest probe PR","statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"},{"state":"SUCCESS"},{"status":"IN_PROGRESS","conclusion":""}]}
JSON
    exit 0 ;;
esac
exit 1
STUB
chmod +x "${stub_dir}/gh"
cat > "${FIXTURE_REPO_DIR}/.devcontainer/devcontainer.json" <<'JSON'
{
  "name": "fleet-man integration fixture (gh stub)",
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
( cd "${FIXTURE_REPO_DIR}" && git add -A && git commit -q -m "fixture: gh stub" )

fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

ghpath=$("${FLEET_BIN}" exec "${inst}" -- sh -c 'command -v gh') || fail "gh is not on the instance's exec PATH"
assert_contains "${ghpath}" "/.itest-bin/gh" "the instance should resolve gh to the stub"

# The trunk has no PR to track, so the probe skips the default branch: work on
# a feature branch, as an agent would.
"${FLEET_BIN}" exec "${inst}" -- git -c safe.directory='*' checkout -q -b feature/pr-probe \
  || fail "could not switch the workspace to a feature branch"
"${FLEET_BIN}" exec "${inst}" -- rm -f /tmp/gh-stub.log

info "spawning the TUI (the probe runs only while a TUI is connected)"
tui_spawn
tui_wait_for "alpha" 15
tui_wait_for "○ idle" 60

# The auto-tag rides the first child row of an expanded instance.
tui_send j
sleep 0.5
tui_send Space
tui_wait_for "+ new session" 15

info "waiting for the PR status from the probe"
if ! tui_wait_for "Checks 2/3" 60; then
  printf -- '--- gh stub log ---\n%s\n--- end ---\n' \
    "$("${FLEET_BIN}" exec "${inst}" -- cat /tmp/gh-stub.log 2>&1 || true)" >&2
  fail "the instance row never showed the PR status"
fi
tui_assert_contains "PR  Accepted  Checks 2/3" "the auto-tag should read PR, the review state and the checks count"

log=$("${FLEET_BIN}" exec "${inst}" -- cat /tmp/gh-stub.log) || fail "the gh stub never ran"
printf -- '--- gh stub log ---\n%s\n--- end ---\n' "${log}"
ws=$("${FLEET_BIN}" exec "${inst}" -- pwd)
assert_contains "${log}" "${ws}|auth status" "the probe should check gh auth from the workspace checkout"
assert_contains "${log}" "${ws}|pr list --state all --head feature/pr-probe" \
  "the probe should list the PRs of the checked-out branch, from the workspace checkout"
assert_contains "${log}" "${ws}|pr view 4242 --json" "the probe should read the open PR's detail"

pass "the gh probe ran in the instance's workspace and the TUI shows the PR status"
