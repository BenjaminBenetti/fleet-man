#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster a fleet's shared folders sync with the daemon host: a host write is in the pods after `fleet sync wait` and a pod's write on the host; a stop flushes, a start waits for the fleet's settings; the host's ~/.claude.json reaches a new instance (never the '{}' seed); a login made on the cluster serves every instance there and stays off the host, as the host's stays off the cluster; `fleet sync status` shows the fleet in sync with its share; destroying the fleet deletes its share once flushed, and the fleet made again under its name is still logged in, on the host and on the cluster.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: the shared-folder sync with a cluster"

fleet="${FIXTURE_REPO_NAME}"
seed() { seed_fleet_settings "${fleet}" true false /home/node false false '["/opt/data"]'; }
run_in() { "${FLEET_BIN}" exec "$1" -- sh -c "$2"; }
host_file() { itest_share_dir "${fleet}" "$1"; }

setup_agent_test
seed

info "the host's ~/.claude.json and Claude login, before the fleet's first instance"
mkdir -p "$(host_file files)" "$(host_file .claude)"
printf '{"numStartups":7,"theme":"dark"}' > "$(host_file files/.claude.json)"
printf '{"claudeAiOauth":{"accessToken":"host-token"}}' > "$(host_file .claude/.credentials.json)"

info "fleet up alpha + beta"
fleet_up alpha
fleet_up beta
alpha="${fleet}/alpha"
beta="${fleet}/beta"

info "~/.claude.json in the instance is the host's, never the '{}' seed"
assert_equals '{"numStartups":7,"theme":"dark"}' "$(run_in "${alpha}" 'cat ~/.claude.json')" "the instance's ~/.claude.json"

info "the host's login stays on the host"
if run_in "${alpha}" 'test -e ~/.claude/.credentials.json' 2>/dev/null; then
  fail "the daemon host's Claude login reached the cluster: logins are per location"
fi

info "a login on the cluster serves every instance there, and stays off the host"
run_in "${alpha}" 'printf "{\"claudeAiOauth\":{\"accessToken\":\"cluster-token\"}}" > ~/.claude/.credentials.json'
assert_contains "$(run_in "${beta}" 'cat ~/.claude/.credentials.json')" "cluster-token" "beta has alpha's login at once (one share)"
itest_share_sync "${fleet}"
assert_contains "$(cat "$(host_file .claude/.credentials.json)")" "host-token" "the host keeps its own login"
store="${HOME}/.fleet/sync/${fleet}/loc/$(itest_k8s_connection)/.claude/.credentials.json"
assert_file_exists "${store}"
assert_contains "$(cat "${store}")" "cluster-token" "the cluster's login, kept on the host"

info "a host write is in the pods after fleet sync wait, and a pod's on the host"
printf 'from-host\n' > "$(host_file .mnt/opt/data/host.txt)"
itest_share_sync "${fleet}"
assert_equals "from-host" "$(run_in "${beta}" 'cat /opt/data/host.txt')" "a host write in the pods after the wait"
run_in "${alpha}" 'echo from-pod > /opt/data/pod.txt'
itest_share_sync "${fleet}"
assert_equals "from-pod" "$(cat "$(host_file .mnt/opt/data/pod.txt)")" "a pod's write on the host after the wait"

info "fleet sync status shows the fleet in sync with its share"
status=$("${FLEET_BIN}" sync status "${fleet}")
assert_contains "${status}" "${fleet}: in sync" "the fleet's sync state"
share=$(itest_k8s_fleet_object "${fleet}" persistentvolumeclaims share)
[ -n "${share}" ] || fail "the fleet has no share"
assert_contains "${status}" "share ${share} on " "its share"
assert_contains "${status}" ".mnt" "its custom mounts' folder"
assert_contains "$("${FLEET_BIN}" sync conflicts "${fleet}")" "No conflict copies." "no conflict copies"

info "a stop flushes: a pod's last write is on the host once the stop returns"
run_in "${alpha}" 'echo last > /opt/data/last.txt'
"${FLEET_BIN}" stop "${alpha}" >/dev/null
assert_equals "last" "$(cat "$(host_file .mnt/opt/data/last.txt)")" "the stopped pod's last write on the host"
"${FLEET_BIN}" stop "${beta}" >/dev/null

info "a start waits for the fleet's settings: one changed while every instance was stopped is in the pod"
printf '{"model":"changed-while-stopped"}' > "$(host_file .claude/settings.json)"
"${FLEET_BIN}" start "${alpha}" >/dev/null
assert_contains "$(run_in "${alpha}" 'cat ~/.claude/settings.json')" "changed-while-stopped" "the settings changed on the host while it was stopped"

info "destroying the fleet flushes, then deletes its share"
"${FLEET_BIN}" destroy "${fleet}" >/dev/null
for _ in $(seq 1 60); do
  [ -z "$(itest_k8s_fleet_object "${fleet}" persistentvolumeclaims share)" ] && break
  sleep 1
done
[ -z "$(itest_k8s_fleet_object "${fleet}" persistentvolumeclaims share)" ] || fail "the destroyed fleet's share ${share} is still there"
assert_file_exists "${store}"

info "made again under its name, the fleet is still logged in, on the host and on the cluster"
seed
fleet_up gamma
assert_contains "$(run_in "${fleet}/gamma" 'cat ~/.claude/.credentials.json')" "cluster-token" "the cluster's login in the new fleet's instance"
assert_contains "$(cat "$(host_file .claude/.credentials.json)")" "host-token" "the host's login"
itest_share_sync "${fleet}"
assert_equals "from-pod" "$(run_in "${fleet}/gamma" 'cat /opt/data/pod.txt')" "the custom mount's content, from the host"

pass "shared folders synced with the cluster; logins per location, kept across a fleet destroyed and made again"
