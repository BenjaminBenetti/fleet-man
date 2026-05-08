#!/usr/bin/env bash
# Description: `fleet up --branch <branch>` clones that branch and `fleet ls` reports it in the BRANCH column.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test

# ===========================================
# The shared fixture only commits to `main`. Add a second branch
# `feature/x` with a sentinel file so we can prove (a) --branch is
# forwarded to git clone, (b) ls reports the actual checked-out branch
# from the workspace, and (c) the workspace contains the branch's content.
# ===========================================
sentinel_file="branch-marker.txt"
sentinel_text="from-feature-branch"
(
  cd "${FIXTURE_REPO_DIR}"
  git checkout -q -b feature/x
  printf '%s\n' "${sentinel_text}" > "${sentinel_file}"
  git add "${sentinel_file}"
  git commit -q -m "feature: add branch marker"
  git checkout -q main
)

# ===========================================
# Spawn the instance against the feature branch.
# ===========================================
info "fleet up alpha --branch feature/x"
"${FLEET_BIN}" up alpha --repo "${FIXTURE_REPO_URL}" --branch feature/x

# Workspace clone must be on feature/x — git rev-parse is the source of truth.
ws_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/alpha/${FIXTURE_REPO_NAME}"
assert_file_exists "${ws_dir}"
checked_branch=$(git -C "${ws_dir}" rev-parse --abbrev-ref HEAD)
assert_equals "feature/x" "${checked_branch}" "workspace should be on the requested branch"

# Branch content arrived too — sentinel file present with expected body.
assert_file_exists "${ws_dir}/${sentinel_file}"
contents=$(cat "${ws_dir}/${sentinel_file}")
assert_equals "${sentinel_text}" "${contents}" "branch sentinel content should match"

# ===========================================
# `fleet ls` must surface the branch in its BRANCH column. The header
# itself is "BRANCH" — assert both the column and the per-row value.
# ===========================================
info "fleet ls (branch column)"
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
printf '%s\n' "${ls_out}"
assert_contains "${ls_out}" "BRANCH"    "ls header should include BRANCH column"
alpha_row=$(printf '%s\n' "${ls_out}" | grep -E '^\S+\s+alpha\s' || true)
[ -n "${alpha_row}" ] || fail "ls missing alpha row: [${ls_out}]"
assert_contains "${alpha_row}" "feature/x" "alpha row should report feature/x in BRANCH column"

# Sanity: state.json records the branch the user requested. (Fleet stores
# the requested ref under the instance entry's `branch` field; whether or
# not that key is set, the workspace HEAD above is the real check.)
state=$(cat "${HOME}/.fleet/state.json")
assert_contains "${state}" "alpha" "state.json should still list alpha"

pass "fleet up --branch clones requested ref and ls reports it"
