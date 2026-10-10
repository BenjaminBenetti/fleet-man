#!/usr/bin/env bash
# Description: with the fleet's image cache on, a docker-in-docker instance's dockerd pulls Docker Hub images through the fleet's registry mirror, still does after a stop/start, the fleet options' Delete cache empties the mirror, and its data outlives a fleet destroy.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; _itest_reclaim_cache_dirs; }
itest_begin

# _itest_reclaim_cache_dirs — the registry writes the cache dir as root; hand it
# back to the test user (with the registry's own image, so nothing is pulled) so
# teardown can wipe ~/.fleet.
_itest_reclaim_cache_dirs() {
  itest_backend_is devcontainer || return 0
  local dir
  dir=$(itest_share_dir "${FIXTURE_REPO_NAME}" .imgcache)
  [ -d "${dir}" ] || return 0
  docker run --rm --entrypoint chown -v "${dir}:/reclaim" registry:2 \
    -R "$(id -u):$(id -g)" /reclaim >/dev/null 2>&1 || true
}

FIXTURE_SRC="${INTEGRATION_DIR}/fixture-dind" setup_test
cat > "${HOME}/.fleet/state.json" <<EOF
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {
      "name": "${FIXTURE_REPO_NAME}",
      "remote": "${FIXTURE_REPO_URL}",
      "settings": { "imageCacheServer": true },
      "instances": []
    }
  }
}
EOF

inst="${FIXTURE_REPO_NAME}/alpha"
in_instance() { "${FLEET_BIN}" exec "${inst}" -- "$@"; }

# mirror_of_inner_dockerd — the registry mirror the instance's dockerd is
# running with (empty until fleet has pointed it at the cache and reloaded it).
mirror_of_inner_dockerd() {
  in_instance docker info --format '{{range .RegistryConfig.Mirrors}}{{.}} {{end}}' 2>/dev/null | sed 's/ *$//'
}
# wait_mirror <when> — wait for the instance's dockerd to get a mirror and set
# mirror_now to it. Not run in $(...), so a timeout fails the test only once.
mirror_now=""
wait_mirror() {
  # fleet configures the mirror once the instance's dockerd is up, polling for
  # it in the background (it starts after the container does).
  local deadline=$(( $(date +%s) + $(_scale_timeout 180) ))
  mirror_now=""
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    # docker info fails until the instance's dockerd answers.
    mirror_now=$(mirror_of_inner_dockerd) || mirror_now=""
    [ -n "${mirror_now}" ] && return 0
    sleep 3
  done
  fail "the instance's dockerd never got a registry mirror (${1}); daemon.json: $(in_instance sh -c 'cat /etc/docker/daemon.json 2>&1')"
}
# catalog — the repositories the mirror holds, asked from inside the instance.
catalog() {
  in_instance sh -c 'curl -fsS "${1%/}/v2/_catalog"' sh "${mirror}"
}

info "fleet up alpha (docker-in-docker, image cache on)"
fleet_up alpha
wait_mirror "after create"
mirror=${mirror_now}
info "inner dockerd mirror: ${mirror}"
if itest_backend_is devcontainer; then
  # Docker reaches the mirror by its container name on the fleet's network.
  assert_equals "http://fleet-${FIXTURE_REPO_NAME}-imgcache:5000/" "${mirror}" "the mirror should be the fleet's cache container"
fi
assert_not_contains "$(catalog)" "library/hello-world" "the mirror should start out without hello-world"

info "pulling hello-world inside the instance"
pull_out=$(in_instance docker pull hello-world 2>&1) || fail "docker pull inside the instance failed: ${pull_out}"
assert_contains "$(catalog)" "library/hello-world" "the pull should go through the fleet's mirror"
run_out=$(in_instance docker run --rm hello-world 2>&1) || fail "docker run inside the instance failed: ${run_out}"
assert_contains "${run_out}" "Hello from Docker!" "the image pulled through the mirror should run"

info "fleet stop + start alpha"
"${FLEET_BIN}" stop "${inst}" >/dev/null
"${FLEET_BIN}" start "${inst}" >/dev/null
wait_mirror "after a stop and start"
assert_equals "${mirror}" "${mirror_now}" "the instance's dockerd should keep the mirror across a restart"
# A repository the mirror has not seen yet, so the catalog shows this pull.
assert_not_contains "$(catalog)" "library/busybox" "the mirror should not hold busybox yet"
pull_out=$(in_instance docker pull busybox 2>&1) || fail "docker pull after a restart failed: ${pull_out}"
assert_contains "$(catalog)" "library/busybox" "pulls after a restart should still go through the fleet's mirror"

# stored <repo> — the mirror's data on the daemon host (docker: the
# bind-mounted .imgcache dir) holds <repo>.
cache_dir=$(itest_share_dir "${FIXTURE_REPO_NAME}" .imgcache)
stored() { [ -n "$(find "${cache_dir}" -path "*$1*" -print -quit 2>/dev/null)" ]; }
cache_name="fleet-${FIXTURE_REPO_NAME}-imgcache"
if itest_backend_is devcontainer; then
  stored hello-world || fail "the mirror should store hello-world in the fleet's cache dir ${cache_dir}"
fi

# Delete cache in the fleet options empties the mirror and brings an empty one
# back, which the instance's dockerd keeps using.
info "Delete cache from the TUI's fleet options"
tui_spawn
tui_wait_for "${FIXTURE_REPO_NAME}" 15 || fail "the TUI never rendered the fleet"
tui_delete_fleet_cache "Docker image cache" "Image cache cleared"
tui_kill
emptied=$(catalog) || fail "the mirror did not answer after Delete cache"
assert_not_contains "${emptied}" "library/hello-world" "Delete cache should empty the mirror"
assert_not_contains "${emptied}" "library/busybox" "Delete cache should empty the mirror"
in_instance docker rmi -f hello-world >/dev/null 2>&1 || true
pull_out=$(in_instance docker pull hello-world 2>&1) || fail "docker pull after Delete cache failed: ${pull_out}"
assert_contains "$(catalog)" "library/hello-world" "pulls after Delete cache should go through the emptied mirror"

if itest_backend_is devcontainer; then
  info "fleet destroy ${FIXTURE_REPO_NAME}"
  "${FLEET_BIN}" destroy "${FIXTURE_REPO_NAME}" >/dev/null
  assert_container_absent "${cache_name}" "destroy should remove the fleet's mirror container"
  stored hello-world || fail "destroy should keep the mirror's data in ${cache_dir}"
fi

pass "image cache: DinD pulls go through the fleet mirror, across restarts; Delete cache empties it; data kept on destroy"
