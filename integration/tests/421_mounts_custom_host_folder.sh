#!/usr/bin/env bash
# itest: k8s
# Description: a custom mount given a daemon-host folder (`fleet mount add --host`) binds that folder in every instance instead of the fleet's .mnt folder: host files show in the instance, its writes reach the folder as the host user, the folder's mode is never changed and no .mnt folder is made; a folder overlapping ~/.fleet is refused, a missing one warns; without --host the next instance is back on the .mnt folder, its writes reaching it (on a cluster also while the older instance keeps the share's sync).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_agent_test
# customMounts=["/opt/data"]; its host folder is set through the CLI below.
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/node false false '["/opt/data"]'

host_dir="${TEST_SCRATCH_DIR}/big-disk"
mkdir -p "${host_dir}"
chmod 0750 "${host_dir}"
printf 'from-host\n' > "${host_dir}/seed.txt"
mnt_dir=$(itest_share_dir "${FIXTURE_REPO_NAME}" .mnt/opt/data)

info "fleet mount add --host"
out=$("${FLEET_BIN}" mount add "${FIXTURE_REPO_NAME}" /opt/data --host "${host_dir}/" 2>&1)
assert_contains "${out}" "Mounted /opt/data" "fleet mount add did not confirm"
listed=$("${FLEET_BIN}" mount ls "${FIXTURE_REPO_NAME}")
assert_contains "${listed}" "${host_dir}" "fleet mount ls does not show the host folder"

info "a host folder over fleet's own state is refused; a missing one warns"
if "${FLEET_BIN}" mount add "${FIXTURE_REPO_NAME}" /opt/other --host "${HOME}/.fleet/workspaces" >/dev/null 2>&1; then
  fail "a host folder inside ~/.fleet was accepted"
fi
warned=$("${FLEET_BIN}" mount add "${FIXTURE_REPO_NAME}" /opt/missing --host "${TEST_SCRATCH_DIR}/nope" 2>&1 >/dev/null)
assert_contains "${warned}" "does not exist on the daemon host" "a missing host folder was not warned of"
"${FLEET_BIN}" mount rm "${FIXTURE_REPO_NAME}" /opt/missing >/dev/null

info "fleet up alpha"
fleet_up alpha
itest_share_sync "${FIXTURE_REPO_NAME}"
inst="${FIXTURE_REPO_NAME}/alpha"

seen=$("${FLEET_BIN}" exec "${inst}" -- cat /opt/data/seed.txt)
assert_contains "${seen}" "from-host" "the host folder's file is not in the instance"
mountinfo=$("${FLEET_BIN}" exec "${inst}" -- cat /proc/self/mountinfo)
assert_contains "${mountinfo}" " /opt/data " "/opt/data is not a mount point"

"${FLEET_BIN}" exec "${inst}" -- sh -c 'printf "from-instance\n" > /opt/data/written.txt'
itest_share_sync "${FIXTURE_REPO_NAME}"
assert_file_exists "${host_dir}/written.txt"
assert_contains "$(cat "${host_dir}/written.txt")" "from-instance" "the instance's write did not reach the host folder"
owner=$(perl -e 'print ((stat shift)[4])' "${host_dir}/written.txt")
assert_equals "$(id -u)" "${owner}" "a file the instance wrote should belong to the host user"
assert_equals "750" "$(perl -e 'printf "%o", (stat shift)[2] & 07777' "${host_dir}")" "the host folder's mode was changed"
[ ! -e "${mnt_dir}" ] || fail "a .mnt folder was made for a mount bound from a host folder"

info "back on the .mnt folder for the next instance"
"${FLEET_BIN}" mount add "${FIXTURE_REPO_NAME}" /opt/data >/dev/null
fleet_up beta
itest_share_sync "${FIXTURE_REPO_NAME}"
if "${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/beta" -- test -e /opt/data/seed.txt; then
  fail "an instance made after the host folder was cleared still binds it"
fi
assert_file_exists "${mnt_dir}"
# alpha (no .mnt folder of its own) is the older instance; on a cluster its
# pod may keep the share's sync, and must keep beta's folders too.
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/beta" -- sh -c 'printf "from-beta\n" > /opt/data/beta.txt'
itest_share_sync "${FIXTURE_REPO_NAME}"
assert_file_exists "${mnt_dir}/beta.txt"
assert_contains "$(cat "${mnt_dir}/beta.txt")" "from-beta" "beta's write to its .mnt folder did not reach the host"
assert_contains "$(cat "${host_dir}/seed.txt")" "from-host" "clearing the host folder touched it"

pass "custom mount host folder bound as is, synced, cleared"
