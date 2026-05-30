#!/usr/bin/env bash
# Description: creating an instance bind-mounts the per-instance control dir
# into the container at /fleet-mounts/control (host <-> instance IPC channel).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

# The default fixture is enough — the control mount is wired by create,
# independent of any fleetLaunch config.
setup_test

info "fleet up alpha"
fleet_up alpha

# Host side: create wires state.ControlDir(fleet, instance) as a bind mount.
# That host dir lives under the instance's workspace tree and must exist
# after up (the host fleet TUI later drops the socket file into it).
host_control_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/alpha/.control"
info "asserting host control dir exists: ${host_control_dir}"
assert_file_exists "${host_control_dir}"
[ -d "${host_control_dir}" ] || fail "host control path is not a directory: ${host_control_dir}"

# Container side: the dir is mounted at the well-known container path
# (control.ContainerMountDir = /fleet-mounts/control) that the in-instance
# launcher dials. The socket file itself is created later by the host TUI,
# so we assert only the mount directory here.
info "asserting /fleet-mounts/control is a directory inside the container"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -d /fleet-mounts/control \
  || fail "/fleet-mounts/control is missing or not a directory inside the instance"

info "asserting /fleet-mounts/control is a real bind mount"
mountinfo=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /proc/self/mountinfo)
assert_contains "${mountinfo}" " /fleet-mounts/control " \
  "/fleet-mounts/control is not a mount point"

# Cross-boundary check: a file the host writes into the control dir is
# visible inside the container through the bind mount (the mechanism the
# real socket relies on).
info "asserting a host-written file in the control dir appears in the container"
probe="host-probe-$$"
: > "${host_control_dir}/${probe}"
container_ls=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- ls /fleet-mounts/control)
assert_contains "${container_ls}" "${probe}" \
  "host-written control file did not appear inside the container"

pass "control mount applied"
