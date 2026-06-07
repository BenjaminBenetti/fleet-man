#!/usr/bin/env bash
# Description: a user-defined custom mount appears in the container at the
# user-supplied path, is a real bind mount backed by the fleet's .mnt
# directory, and shares writes across the host boundary like the built-in
# Claude/Codex/Gh mounts.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_agent_test
# claude=false codex=false homedir=/home/node gh=false buildkit=false
# customMounts=["/opt/data"]
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/node false false '["/opt/data"]'

info "seeding host-side custom mount dir with a stand-in file"
host_mnt_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/.mnt/opt/data"
mkdir -p "${host_mnt_dir}"
printf "from-host\n" > "${host_mnt_dir}/seed.txt"

info "fleet up alpha (custom mount /opt/data enabled)"
fleet_up alpha

info "asserting /opt/data is a directory inside the container"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -d /opt/data \
  || fail "/opt/data is missing or not a directory"

info "asserting /opt/data is a real bind mount"
mountinfo=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /proc/self/mountinfo)
assert_contains "${mountinfo}" " /opt/data " "/opt/data is not a mount point"

info "asserting host-seeded file is visible inside the container"
container_seed=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /opt/data/seed.txt)
assert_contains "${container_seed}" "from-host" \
  "host-seeded file did not appear inside the container"

info "asserting a container-side write propagates back to the host"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- \
  sh -c 'printf "from-container\n" > /opt/data/written.txt'
assert_file_exists "${host_mnt_dir}/written.txt"
host_written=$(cat "${host_mnt_dir}/written.txt")
assert_contains "${host_written}" "from-container" \
  "container-side write did not reach the host file"

info "asserting the host path lives under the fleet's .mnt directory"
assert_file_exists "${host_mnt_dir}"

pass "custom mount applied"
