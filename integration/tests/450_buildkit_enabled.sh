#!/usr/bin/env bash
# Description: enabling buildkit_server boots a shared buildkit container, exposes a working buildkitd socket, and bind-mounts it into the instance.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

setup_test
# claude=false codex=false homedir=/home/vscode gh=false buildkit=true
seed_fleet_settings "${FIXTURE_REPO_NAME}" false false /home/vscode false true

bk_container="fleet-${FIXTURE_REPO_NAME}-buildkit"
host_bk_dir="${HOME}/.fleet/workspaces/${FIXTURE_REPO_NAME}/.buildkit"
host_sock="${host_bk_dir}/buildkitd.sock"

info "fleet up alpha (buildkit enabled)"
fleet_up alpha

# 1. The shared buildkit container is up. (fleet up blocks through the docker
#    run + image pull, so it should already be running; poll briefly to absorb
#    slow-CI boot.)
info "asserting the shared buildkit container is running"
running="missing"
for _ in $(seq 1 "$(_scale_timeout 30)"); do
  running=$(docker inspect -f '{{.State.Running}}' "${bk_container}" 2>/dev/null || echo missing)
  [ "${running}" = "true" ] && break
  sleep 1
done
assert_equals "true" "${running}" "shared buildkit container ${bk_container} is not running"

# 2. The buildkitd socket and the bound cache dir exist on the host.
info "asserting host socket + cache dir exist under .buildkit/"
for _ in $(seq 1 "$(_scale_timeout 15)"); do
  [ -S "${host_sock}" ] && break
  sleep 1
done
[ -S "${host_sock}" ] || fail "host buildkitd.sock missing or not a socket: ${host_sock}"
assert_file_exists "${host_bk_dir}/cache"

# 3. The server is actually serving: buildctl (shipped in the moby/buildkit
#    image) lists at least one worker over the unix socket.
info "asserting the buildkit server answers buildctl debug workers"
workers=$(docker exec "${bk_container}" \
  buildctl --addr unix:///run/fleet-buildkit/buildkitd.sock debug workers 2>&1) \
  || fail "buildctl debug workers failed: ${workers}"
assert_contains "${workers}" "linux/" "buildkit server reported no usable worker platform"

# 4. The socket is bind-mounted into the instance at the shared path.
info "asserting the socket is bind-mounted into the instance"
"${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- test -S /run/fleet-buildkit/buildkitd.sock \
  || fail "buildkitd.sock is not available inside the instance"
mountinfo=$("${FLEET_BIN}" exec "${FIXTURE_REPO_NAME}/alpha" -- cat /proc/self/mountinfo)
assert_contains "${mountinfo}" " /run/fleet-buildkit " "/run/fleet-buildkit is not a mount point in the instance"

# 5. The debian fixture has no docker/buildx, so ConfigureInstanceBuildx is a
#    silent no-op — provisioning must still succeed and the instance is usable.
info "asserting the instance provisioned cleanly despite no docker/buildx in the image"
ls_out=$("${FLEET_BIN}" ls "${FIXTURE_REPO_NAME}")
assert_contains "${ls_out}" "alpha" "instance is missing after buildkit setup"

pass "buildkit enabled: server booted, socket healthy and mounted into the instance"
