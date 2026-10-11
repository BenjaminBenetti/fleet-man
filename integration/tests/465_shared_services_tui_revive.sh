#!/usr/bin/env bash
# Description: a TUI connecting brings back every shared service (buildkit, deb cache, image cache) a fleet with an instance has on, after they were removed behind fleet's back, and starts none for a fleet with no instance.
# itest: k8s
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
# The instance-less fleet sorts before the fixture fleet, so a reconcile that
# wrongly took it on would start its services before the fixture fleet's.
idle_fleet="itest-a-empty"
if itest_backend_is kubernetes; then
  # Shared Buildkit does not run on a cluster yet (it needs per-instance
  # TLS); the caches are Deployments in the connection's namespace.
  services='"debCacheServer": true, "imageCacheServer": true'
  wanted="debcache imagecache"
else
  services='"buildkitServer": true, "debCacheServer": true, "imageCacheServer": true'
  wanted="buildkit debcache imagecache"
fi
cat > "${HOME}/.fleet/state.json" <<STATE
{
  "fleets": {
    "${FIXTURE_REPO_NAME}": {
      "name": "${FIXTURE_REPO_NAME}",
      "remote": "${FIXTURE_REPO_URL}",
      "settings": { ${services} },
      "instances": []
    },
    "${idle_fleet}": {
      "name": "${idle_fleet}",
      "remote": "${FIXTURE_REPO_URL}",
      "settings": { ${services} },
      "instances": []
    }
  }
}
STATE

info "fleet up alpha (${wanted} on)"
fleet_up alpha
for svc in ${wanted}; do
  itest_wait_service "${FIXTURE_REPO_NAME}" "${svc}" running 180
done

info "removing the service servers behind fleet's back"
for svc in ${wanted}; do
  itest_service_remove "${FIXTURE_REPO_NAME}" "${svc}"
  itest_wait_service "${FIXTURE_REPO_NAME}" "${svc}" absent 60
done

info "opening the TUI"
tui_spawn
tui_wait_for "${FIXTURE_REPO_NAME}" 15

info "waiting for the TUI-connect reconcile to bring every service back"
for svc in ${wanted}; do
  itest_wait_service "${FIXTURE_REPO_NAME}" "${svc}" running 180
done

info "asserting the instance-less fleet got no service servers"
for svc in buildkit debcache imagecache; do
  st=$(itest_service_status "${idle_fleet}" "${svc}")
  [ "${st}" = "absent" ] || fail "${idle_fleet} has no instance, but the reconcile started its ${svc} server (${st})"
done
if itest_backend_is kubernetes; then
  # Nor any object of its own on the cluster: it has no instance there.
  left=$(itest_k8s_fleet_objects "${idle_fleet}")
  [ -z "${left}" ] || fail "${idle_fleet} has no instance, but it has objects on the cluster: $(printf '%s' "${left}" | tr '\n' ' ')"
fi

pass "TUI connect revives a fleet's shared services and leaves an empty fleet alone"
