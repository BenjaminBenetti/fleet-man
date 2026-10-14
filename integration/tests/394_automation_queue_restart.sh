#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, webhook firings queued behind the connection's max_instances survive a fleetd restart: with one instance allowed, a burst of three runs one and queues two (202, kept in the watch list); fleetd killed with SIGKILL once the first run's agent is up and started again restores them ("queued=2"), and as each run is reaped the next starts, never two at once, every event getting its run with none refused, dropped or expired. On devcontainer it passes saying so: the queue is the cluster limits'.
set -euo pipefail

source "$(dirname "$0")/../common.sh"

GW_PID=""
itest_cleanup() {
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  if [ -n "${GW_PID}" ]; then
    kill "${GW_PID}" 2>/dev/null || true
    wait "${GW_PID}" 2>/dev/null || true
  fi
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: firings queue behind a cluster connection's limits"

# A run is reaped a minute after its agent goes idle; the scheduler looks
# every 5s. The daemon inherits these.
export FLEET_AUTOMATION_IDLE_TIMEOUT="60s"
export FLEET_AUTOMATION_TICK_INTERVAL="5s"
setup_automation_test
kill_daemons() {
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  local deadline=$(( $(date +%s) + $(_scale_timeout 10) ))
  while [ "$(server_count)" != "0" ] && [ "$(date +%s)" -lt "${deadline}" ]; do sleep 0.2; done
}
kill_daemons
FLEET="${FIXTURE_REPO_NAME}"
cat > "${HOME}/.fleet/state.json" <<EOF
{ "fleets": { "${FLEET}": { "name": "${FLEET}", "remote": "${FIXTURE_REPO_URL}", "settings": {}, "instances": [] } } }
EOF
log="${HOME}/.fleet/fleet.log"
WATCH="${HOME}/.fleet/automation/watch.json"

free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'; }

info "a gateway for webhook deliveries"
PUBLIC_PORT="$(free_port)"
GRPC_PORT="$(free_port)"
while [ "${GRPC_PORT}" = "${PUBLIC_PORT}" ]; do GRPC_PORT="$(free_port)"; done
cfg="${HOME}/.fleet/config.json"
[ -f "${cfg}" ] || echo '{}' > "${cfg}"
jq --arg gw "http://127.0.0.1:${GRPC_PORT}" '.remote_mcp_settings = {webhook_enabled: true, gateway_url: $gw}' "${cfg}" > "${cfg}.tmp"
mv "${cfg}.tmp" "${cfg}"
gw_log="${TEST_SCRATCH_DIR}/gateway.log"
"${FLEET_BIN}" gateway --public-url "http://127.0.0.1:${PUBLIC_PORT}" --public-addr "127.0.0.1:${PUBLIC_PORT}" \
  --grpc-addr "127.0.0.1:${GRPC_PORT}" >"${gw_log}" 2>&1 &
GW_PID=$!
deadline=$(( $(date +%s) + $(_scale_timeout 10) ))
until curl -fsS "http://127.0.0.1:${PUBLIC_PORT}/healthz" >/dev/null 2>&1; do
  kill -0 "${GW_PID}" 2>/dev/null || fail "the gateway exited: $(cat "${gw_log}")"
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the gateway never answered /healthz: $(cat "${gw_log}")"
  sleep 0.2
done

info "one instance allowed on the connection; an agent and its webhook trigger"
out=$("${FLEET_BIN}" cluster set itest max_instances=1 automation_fire_interval=0 automation_max_per_agent=0 \
  resources.cpu_request=100m resources.memory_request=256Mi 2>&1) || fail "fleet cluster set itest: ${out}"
out=$("${FLEET_BIN}" agent create "${FLEET}" queuer --cluster itest \
  --command "ANTHROPIC_API_KEY=itest claude '\${PROMPT}'" 2>&1) || fail "agent create queuer: ${out}"
out=$("${FLEET_BIN}" trigger create "${FLEET}" queue --type webhook --agent queuer \
  --webhook-name itest-queue --filter-type regex --regex '.*' --prompt QUEUED 2>&1) || fail "trigger create queue: ${out}"
session_file="${HOME}/.fleet/gateway_session.json"
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
until [ -f "${session_file}" ] && grep -q '"public_webhook_url"' "${session_file}"; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "fleetd never registered a webhook URL with the gateway"
  sleep 0.2
done
webhook_base=$(grep -oE '"public_webhook_url"[[:space:]]*:[[:space:]]*"[^"]+"' "${session_file}" | sed -E 's/.*"([^"]+)"$/\1/')
deliver() {
  curl -sS -o "${TEST_SCRATCH_DIR}/reply.$1" -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    --data-binary "{\"n\":$1}" "${webhook_base}/itest-queue" 2>/dev/null || printf '000'
}

info "a burst of three: one runs, two wait"
code=""
for _ in $(seq 1 "$(_scale_timeout 10)"); do
  code=$(deliver 1)
  [ "${code}" = 200 ] && break
  sleep 1
done
assert_equals "200" "${code}" "the first delivery fires: $(cat "${TEST_SCRATCH_DIR}/reply.1" 2>/dev/null)"
for n in 2 3; do
  assert_equals "202" "$(deliver "${n}")" "delivery ${n} waits for max_instances: $(cat "${TEST_SCRATCH_DIR}/reply.${n}" 2>/dev/null)"
done
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
until [ "$(jq '.queued | length' "${WATCH}" 2>/dev/null || echo 0)" = 2 ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the watch list does not hold the two queued firings: $(cat "${WATCH}" 2>/dev/null)"
  sleep 0.5
done

# The first run up and its agent launched (a create a restart interrupts is
# destroyed, not run: its firing would be lost by design).
deadline=$(( $(date +%s) + $(_scale_timeout 300) ))
until [ "$(jq '[.runs[]? | select(.launched == true)] | length' "${WATCH}" 2>/dev/null || echo 0)" -ge 1 ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the first run never launched its agent: $(cat "${WATCH}" 2>/dev/null)"
  sleep 1
done

info "fleetd killed and started again: the queue comes back"
old_pid=$(pgrep -f "${FLEET_BIN} server" | head -n 1 || true)
[ -n "${old_pid}" ] || fail "no daemon to kill"
pkill -KILL -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
deadline=$(( $(date +%s) + $(_scale_timeout 10) ))
while [ "$(server_count)" != "0" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "fleetd survived SIGKILL"
  sleep 0.2
done
"${FLEET_BIN}" ls "${FLEET}" >/dev/null 2>&1 || true
new_pid=$(pgrep -f "${FLEET_BIN} server" | head -n 1 || true)
[ -n "${new_pid}" ] && [ "${new_pid}" != "${old_pid}" ] || fail "fleetd did not come back as a new process"
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
until grep 'automation: watch list restored' "${log}" | grep "pid=${new_pid} " | grep -q 'queued=2'; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the restarted fleetd did not restore the two queued firings: $(grep 'watch list restored' "${log}" | tail -n 2)"
  sleep 0.5
done

info "each run reaped lets the next start: every event gets its run, one at a time"
# queuers — the runs holding the cluster now (what max_instances counts).
queuers() {
  jq -r --arg f "${FLEET}" '[.fleets[$f].instances[]? | select((.name | startswith("queuer-")) and (.kube_key // "") != "" and .status != "deleting")] | length' \
    "${HOME}/.fleet/state.json" 2>/dev/null || echo 0
}
most=0
started=0
logs=""
deadline=$(( $(date +%s) + $(_scale_timeout 900) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  now=$(queuers)
  [ "${now}" -le "${most}" ] || most=${now}
  logs=$("${FLEET_BIN}" trigger logs "${FLEET}" queue 2>/dev/null || true)
  started=$(printf '%s\n' "${logs}" | grep -cE '^# outcome: [^ ]+ queuer: instance queuer-[0-9]+-[0-9]+ \(☸ itest\)$' || true)
  [ "${started}" -ge 3 ] && break
  sleep 2
done
printf '%s\n' "${logs}" | grep '^# outcome:' || true
assert_equals "3" "${started}" "every delivery gets its run across the restart"
[ "${most}" -le 1 ] || fail "${most} runs held the cluster at once, over max_instances=1"
assert_equals "2" "$(printf '%s\n' "${logs}" | grep -c 'queuer: queued: ' || true)" "two firings waited in the queue"
assert_equals "0" "$(printf '%s\n' "${logs}" | grep -cE 'queuer: (refused|dropped|expired|not created)' || true)" "no run is lost"

pass "queued firings survive a fleetd restart and each gets its run under max_instances"
