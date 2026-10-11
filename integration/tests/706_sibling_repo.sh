#!/usr/bin/env bash
# itest: k8s
# Description: repos cloned or created next to the workspace in /workspaces (the workspace mount's parent) keep their commits, uncommitted changes and git integrity across stop/start, beside an untouched workspace.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
fleet_up alpha

inst="${FIXTURE_REPO_NAME}/alpha"
ws="/workspaces/${FIXTURE_REPO_NAME}"
in_instance() { "${FLEET_BIN}" exec "${inst}" -- "$@"; }
git_in() {  # <dir> <git-args...>
  local dir="$1"
  shift
  in_instance git -c user.name=itest -c user.email=itest@fleet-man.local -C "${dir}" "$@"
}

# Two siblings, made the two usual ways: a clone (here of the workspace's own
# repo, so no network is needed) and a fresh `git init`. Each gets a commit of
# its own and an uncommitted change. /workspaces itself belongs to root (the
# workspace mount created it), so the user makes their dirs with sudo first.
info "cloning and creating sibling repos in /workspaces"
in_instance sudo sh -c 'mkdir /workspaces/sibling-clone /workspaces/sibling-init && chown vscode:vscode /workspaces/sibling-clone /workspaces/sibling-init'
in_instance git clone -q "${ws}" /workspaces/sibling-clone
in_instance sh -c 'printf "sibling commit\n" > /workspaces/sibling-clone/SIBLING.txt'
git_in /workspaces/sibling-clone add SIBLING.txt
git_in /workspaces/sibling-clone commit -q -m "sibling: own commit"
in_instance sh -c 'printf "sibling uncommitted\n" > /workspaces/sibling-clone/WIP.txt'

in_instance sh -c 'printf "fresh repo\n" > /workspaces/sibling-init/NOTES.md'
git_in /workspaces/sibling-init init -q -b main
git_in /workspaces/sibling-init add NOTES.md
git_in /workspaces/sibling-init commit -q -m "fresh: first commit"
in_instance sh -c 'printf "fresh edit\n" >> /workspaces/sibling-init/NOTES.md'

snapshot() {
  printf 'ls=%s\n' "$(in_instance sh -c 'ls -1 /workspaces | sort | tr "\n" " "')"
  printf 'owners=%s\n' "$(in_instance stat -c '%n %U:%G %a' /workspaces /workspaces/sibling-clone /workspaces/sibling-init | tr '\n' ' ')"
  printf 'clone-head=%s\n' "$(git_in /workspaces/sibling-clone log -1 --format=%s)"
  printf 'clone-status=%s\n' "$(git_in /workspaces/sibling-clone status --porcelain | tr '\n' ' ')"
  printf 'init-head=%s\n' "$(git_in /workspaces/sibling-init log -1 --format=%s)"
  printf 'init-status=%s\n' "$(git_in /workspaces/sibling-init status --porcelain | tr '\n' ' ')"
  printf 'ws-status=%s\n' "$(itest_workspace_git "${inst}" status --porcelain | tr '\n' ' ')"
}
before=$(snapshot)
printf '%s\n' "${before}"
assert_contains "${before}" "ls=${FIXTURE_REPO_NAME} sibling-clone sibling-init " \
  "/workspaces should hold the workspace and both siblings"
assert_contains "${before}" "clone-head=sibling: own commit" "the cloned sibling should have its commit"
assert_contains "${before}" "init-head=fresh: first commit" "the fresh sibling should have its commit"
printf '%s\n' "${before}" | grep -qx 'ws-status=' \
  || fail "the siblings must not show up in the workspace's own status: ${before}"

info "fleet stop + start alpha"
"${FLEET_BIN}" stop "${inst}" >/dev/null
"${FLEET_BIN}" start "${inst}" >/dev/null

after=$(snapshot)
assert_equals "${before}" "${after}" "the sibling repos should come back exactly as they were"
git_in /workspaces/sibling-clone fsck --no-progress >/dev/null 2>&1 \
  || fail "the cloned sibling fails git fsck after a stop and start"
git_in /workspaces/sibling-init fsck --no-progress >/dev/null 2>&1 \
  || fail "the fresh sibling fails git fsck after a stop and start"
assert_equals "sibling uncommitted" "$(in_instance cat /workspaces/sibling-clone/WIP.txt)" \
  "the cloned sibling's uncommitted file should survive"

pass "sibling repos in /workspaces survive stop/start"
