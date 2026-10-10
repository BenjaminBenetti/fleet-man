#!/usr/bin/env bash
# Description: creating an instance mounts the per-instance control dir into
# the container at /fleet-mounts/control (host <-> instance IPC channel).
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

# The default fixture is enough — the control mount is wired by create,
# independent of any fleetLaunch config.
setup_test

info "fleet up alpha"
fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

# The control dir exists after up (the daemon later drops the socket file into
# it). Devcontainer: create wires state.ControlDir(fleet, instance), a host dir
# under the instance's workspace tree, as a bind mount.
info "asserting the instance's control dir exists"
itest_control_test "${inst}" -d . || fail "the instance's control dir is missing or not a directory"

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
# visible inside the container through the bind mount (the mechanism the real
# socket relies on). Only devcontainer has a host side to the control dir:
# clusters keep it in the pod and relay its sockets.
if itest_backend_is devcontainer; then
  info "asserting a host-written file in the control dir appears in the container"
  probe="host-probe-$$"
  : > "$(itest_host_control_dir "${inst}")/${probe}"
  container_ls=$("${FLEET_BIN}" exec "${inst}" -- ls /fleet-mounts/control)
  assert_contains "${container_ls}" "${probe}" \
    "host-written control file did not appear inside the container"
fi

pass "control mount applied"
