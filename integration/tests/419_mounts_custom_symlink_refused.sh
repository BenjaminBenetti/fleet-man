#!/usr/bin/env bash
# itest: k8s
# Description: a custom mount whose host folder (.mnt/<path>) was replaced
# with a symlink is refused, not followed: fleet up fails naming the host-folder
# option, the instance is FAILED, and the link's target keeps its mode and
# contents. A symlink a user keeps inside a mount folder still provisions.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
# claude=false codex=false homedir=/home/node gh=false buildkit=false
# customMounts=["/opt/data"]
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/node false false '["/opt/data"]'

info "replacing the custom mount's host folder with a symlink to a private dir"
outside="${TEST_SCRATCH_DIR}/big-disk"
mkdir -p "${outside}"
chmod 700 "${outside}"
printf "private\n" > "${outside}/keep.txt"
mnt_dir=$(itest_share_dir "${FIXTURE_REPO_NAME}" .mnt/opt/data)
mkdir -p "$(dirname "${mnt_dir}")"
ln -s "${outside}" "${mnt_dir}"

info "fleet up alpha must fail with the refusal"
if output=$(fleet_up alpha 2>&1); then
  fail "fleet up provisioned through a symlinked custom mount folder"
fi
refusal="custom mount /opt/data: ~/.fleet/workspaces/${FIXTURE_REPO_NAME}/.mnt/opt/data is a symlink; pick a host folder for this mount in the fleet options instead"
assert_contains "${output}" "${refusal}" "fleet up did not print the refusal"

info "asserting the instance is FAILED and carries the refusal"
ls_line=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}" | grep -w alpha || true)
assert_contains "${ls_line}" "failed" "alpha is not FAILED: ${ls_line}"
assert_contains "$(cat "${HOME}/.fleet/state.json")" "is a symlink; pick a host folder" \
  "the instance error does not carry the refusal"

info "asserting the link and its target were left alone"
[[ -L "${mnt_dir}" ]] || fail "the symlink at ${mnt_dir} was replaced"
assert_equals "700" "$(file_mode "${outside}")" "the link target was chmodded"
assert_equals "private" "$(cat "${outside}/keep.txt")" "the link target's content changed"
[[ "$(ls -A "${outside}")" == "keep.txt" ]] || fail "fleet wrote into the link target: $(ls -A "${outside}")"

info "a real folder with a user symlink inside it provisions"
rm "${mnt_dir}"
mkdir -p "${mnt_dir}"
ln -s "${outside}" "${mnt_dir}/big-disk"
fleet_up beta
itest_share_sync "${FIXTURE_REPO_NAME}"
assert_equals "private" "$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/beta" -- sh -c 'test -L /opt/data/big-disk && echo private')" \
  "the user's symlink inside the mount folder is missing in the container"
assert_equals "700" "$(file_mode "${outside}")" "the target of a user's symlink was chmodded"

pass "symlinked custom mount folder refused"
