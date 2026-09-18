#!/usr/bin/env bash
# Description: `fleet up <fleet>/<name> --repo file:///dir` copies a local template dir into each instance instead of cloning.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test

# ===========================================
# A template is any local directory with a devcontainer.json. Build one
# from the fixture WITHOUT a .git so the test proves the workspace comes
# from a copy, not a clone. A sentinel file lets us check content + isolation.
# ===========================================
template_dir="${TEST_SCRATCH_DIR}/scratch-template"
rm -rf "${template_dir}"
mkdir -p "${template_dir}"
cp -r "${FIXTURE_SRC}/." "${template_dir}/"
printf 'from-template\n' > "${template_dir}/sentinel.txt"
template_url="file://${template_dir}"

# ===========================================
# Error paths first — each must fail BEFORE any instance record exists.
# ===========================================
info "fleet up alpha --repo file://... without an explicit fleet name must fail"
set +e
out=$("${FLEET_BIN}" up alpha --repo "${template_url}" 2>&1); rc=$?
set -e
printf '%s\n' "${out}"
[ "${rc}" -ne 0 ] || fail "template --repo without <fleet>/<name> should be rejected"
assert_contains "${out}" "<fleet>/alpha" "error should show the fleet/instance form"

info "fleet up scratch/alpha --branch main on a template must fail"
set +e
out=$("${FLEET_BIN}" up scratch/alpha --repo "${template_url}" --branch main 2>&1); rc=$?
set -e
printf '%s\n' "${out}"
[ "${rc}" -ne 0 ] || fail "--branch on a template fleet should be rejected"
assert_contains "${out}" "branch" "error should mention branch"

info "fleet up scratch/alpha --repo file:///missing must fail"
set +e
out=$("${FLEET_BIN}" up scratch/alpha --repo "file://${TEST_SCRATCH_DIR}/does-not-exist" 2>&1); rc=$?
set -e
printf '%s\n' "${out}"
[ "${rc}" -ne 0 ] || fail "missing template dir should be rejected"
assert_contains "${out}" "template dir" "error should name the template dir problem"

# Nothing may be left behind by a rejected create: no instance record (the
# state file may not even exist yet) and no workspace dir.
if [ -f "${HOME}/.fleet/state.json" ]; then
  assert_not_contains "$(cat "${HOME}/.fleet/state.json")" '"alpha"' "rejected creates must not leave an instance record"
fi
assert_file_absent "${HOME}/.fleet/workspaces/scratch"

info "fleet up scratch/alpha --repo file://\$HOME (template contains the workspace dir) must fail"
set +e
out=$("${FLEET_BIN}" up scratch/alpha --repo "file://${HOME}" 2>&1); rc=$?
set -e
printf '%s\n' "${out}"
[ "${rc}" -ne 0 ] || fail "a template that contains ~/.fleet/workspaces should be rejected"
assert_contains "${out}" "copied into itself" "error should explain the self-containment"
assert_file_absent "${HOME}/.fleet/workspaces/scratch"

# ===========================================
# Happy path: explicit fleet name + template URL.
# ===========================================
info "fleet up scratch/alpha --repo ${template_url}"
"${FLEET_BIN}" up scratch/alpha --repo "${template_url}"

ws_dir="${HOME}/.fleet/workspaces/scratch/alpha/scratch"
assert_file_exists "${ws_dir}/sentinel.txt"
assert_file_exists "${ws_dir}/.devcontainer/devcontainer.json"
assert_file_absent "${ws_dir}/.git" "workspace must be a copy of the template, not a git clone"
assert_equals "from-template" "$(cat "${ws_dir}/sentinel.txt")" "template content should be copied"

ls_out=$("${FLEET_BIN}" ls scratch)
printf '%s\n' "${ls_out}"
assert_contains "${ls_out}" "scratch"  "fleet name missing from ls"
assert_contains "${ls_out}" "alpha"    "instance missing from ls"
assert_contains "${ls_out}" "running"  "instance should be running"

state=$(cat "${HOME}/.fleet/state.json")
assert_contains "${state}" "\"remote\": \"${template_url}\"" "fleet record should keep the file:// remote"

# ===========================================
# Isolation: editing the instance never touches the template, and a second
# instance (no --repo: the fleet's recorded remote is reused) starts from
# the pristine template again.
# ===========================================
info "editing the workspace copy must not modify the template"
printf 'edited-in-instance\n' > "${ws_dir}/sentinel.txt"
assert_equals "from-template" "$(cat "${template_dir}/sentinel.txt")" "template must stay pristine"

info "fleet up scratch/beta (reuses the fleet's template remote)"
"${FLEET_BIN}" up scratch/beta
beta_ws="${HOME}/.fleet/workspaces/scratch/beta/scratch"
assert_equals "from-template" "$(cat "${beta_ws}/sentinel.txt")" "second instance should start from the pristine template"

pass "fleet up from a file:// template copies the dir per instance"
