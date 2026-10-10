#!/usr/bin/env bash
# Description: a TUI connecting brings back every shared service (buildkit, deb cache, image cache) a fleet with a devcontainer instance has on, after they were removed behind fleet's back, and starts none for a fleet with no instance.
# itest: devcontainer-only: the shared services are host docker containers removed with docker rm; on a cluster they are per-fleet Deployments the reconcile re-applies (P5.0 ports this test)
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_cleanup() { tui_kill; }
itest_begin

setup_test
# The instance-less fleet sorts before the fixture fleet, so a reconcile that
# wrongly took it on would start its services before the fixture fleet's.
idle_fleet="itest-a-empty"
services='"buildkitServer": true, "debCacheServer": true, "imageCacheServer": true'
cat > "${HOME}/.fleet/state.json" <<EOF
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
EOF

# service_containers <fleet> — the fleet's shared service container names.
service_containers() {
  printf '%s\n' "fleet-$1-buildkit" "fleet-$1-aptcache" "fleet-$1-imgcache"
}

# wait_services_running <fleet> <seconds> — every service container of <fleet>
# is running within the budget.
wait_services_running() {
  local c state deadline
  deadline=$(( $(date +%s) + $(_scale_timeout "$2") ))
  for c in $(service_containers "$1"); do
    state=missing
    while [ "$(date +%s)" -lt "${deadline}" ]; do
      state=$(docker inspect -f '{{.State.Running}}' "${c}" 2>/dev/null) || state=missing
      [ "${state}" = "true" ] && break
      sleep 1
    done
    [ "${state}" = "true" ] || fail "shared service container ${c} not running (state: ${state})"
  done
}

info "fleet up alpha (buildkit, deb cache and image cache on)"
fleet_up alpha
wait_services_running "${FIXTURE_REPO_NAME}" 60

info "removing the service containers behind fleet's back"
# shellcheck disable=SC2046 # one word per container name
docker rm -f $(service_containers "${FIXTURE_REPO_NAME}") >/dev/null
for c in $(service_containers "${FIXTURE_REPO_NAME}"); do
  if docker inspect "${c}" >/dev/null 2>&1; then
    fail "${c} still exists after docker rm -f"
  fi
done

info "opening the TUI"
tui_spawn
tui_wait_for "${FIXTURE_REPO_NAME}" 15

info "waiting for the TUI-connect reconcile to bring every service back"
wait_services_running "${FIXTURE_REPO_NAME}" 120

info "asserting the instance-less fleet got no service containers"
for c in $(service_containers "${idle_fleet}"); do
  if docker inspect "${c}" >/dev/null 2>&1; then
    fail "${idle_fleet} has no instance, but the reconcile started ${c}"
  fi
done

pass "TUI connect revives a fleet's shared services and leaves an empty fleet alone"
