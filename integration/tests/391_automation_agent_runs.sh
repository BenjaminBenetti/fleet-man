#!/usr/bin/env bash
# itest: k8s
# Description: end-to-end automation (issue #188) — a fired Schedule trigger
# spawns an instance, launches the agent (which RUNS with the trigger's prompt),
# and then the idle instance is reaped; `fleet trigger logs` records each
# outcome (the instance it got, then the reap). The fixture installs a fake
# `claude` under ~/.local/bin (on PATH only via .bashrc, like the real install)
# that records its args to /tmp/automation_agent_out then sleeps; we assert the
# prompt reaches it AND that the idle agent's instance is torn down. This is the
# real proof of the whole scheduler -> spawn -> launch -> reap lifecycle.
#
# On kubernetes the agent runs on the run's connection, and the cluster limits
# are proven too: a burst of 5 webhook deliveries to one trigger is queued
# (202) and every event gets its run, spaced by the connection's fire interval
# and at most automation_max_per_agent at once; a full queue answers 429 with
# Retry-After; an agent whose tool has no login on the cluster, and one whose
# connection was forgotten, are refused with the reason in its trigger log.
set -euo pipefail

source "$(dirname "$0")/../common.sh"

GW_PID=""
GONE_NS=""
# Kill our daemon on exit so its short idle-timeout env (set below) can't bleed
# into a later test's scheduler (mirrors 601/611), and the gateway and the
# second connection's namespace the kubernetes half makes.
itest_cleanup() {
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  if [ -n "${GW_PID}" ]; then
    kill "${GW_PID}" 2>/dev/null || true
    wait "${GW_PID}" 2>/dev/null || true
  fi
  if [ -n "${GONE_NS}" ]; then
    _be_kubectl delete namespace "${GONE_NS}" --wait=false >/dev/null 2>&1 || true
  fi
}
itest_begin


# Shorten the scheduler's idle timeout + tick so the reap half of the lifecycle
# verifies in ~a minute instead of the production 2m/30s. The daemon inherits
# this env (spawn.go: cmd.Env = os.Environ()), so it must be exported BEFORE the
# `fleet ls` below starts the daemon + scheduler. The idle clock only starts once
# the agent launches (post-provision), so 60s leaves a wide margin to read the
# agent's output before the instance is reaped, even on a slow runner.
export FLEET_AUTOMATION_IDLE_TIMEOUT="60s"
export FLEET_AUTOMATION_TICK_INTERVAL="5s"

setup_automation_test

# A daemon lingers across tests (teardown wipes ~/.fleet but never kills the
# Setsid daemon). A prior test's daemon still has the PRODUCTION 2m idle timeout
# and its own scheduler — left alive it would race ours (firing the trigger,
# reaping on the wrong clock). Kill it so the `fleet ls` below cold-spawns
# exactly one fresh daemon that inherits our short-timeout env.
kill_daemons() {
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  local deadline=$(( $(date +%s) + $(_scale_timeout 5) ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    [ "$(server_count)" = "0" ] && break
    sleep 0.2
  done
}
info "killing any lingering daemon so we cold-spawn one with our env"
kill_daemons

FLEET="${FIXTURE_REPO_NAME}"
MARKER="AUTOMATIONPROMPTMARKER42"

# The agent's target: devcontainer, or the run's connection on kubernetes. Its
# command sets the tool's API key, which signs it in wherever it runs (the
# automation login check refuses a claude agent on a cluster where Claude was
# never logged in for the fleet; the fake claude ignores the key).
agent_target='"backend": "devcontainer"'
target_label="devcontainer"
if itest_backend_is kubernetes; then
  agent_target="\"backend\": \"kubernetes\", \"kube_cluster\": \"$(_be_cluster_id)\""
  target_label="☸ itest"
fi

# A cron matching the CURRENT minute fires exactly once (its only match is this
# minute of this hour; next is tomorrow), so the daemon's immediate startup tick
# fires it without spawning a new instance every minute. The ONLY thing that fires
# it is that startup tick evaluating the cron against the wall clock, so the daemon
# must boot while we're still inside the seeded minute. Align to the top of a fresh
# minute first (unless we're already near it) so there's ~a full minute of margin —
# far more than the sub-second seed + daemon cold-spawn + first tick consumes, even
# on a loaded runner. (Plain wall-clock alignment, not a scaled timeout budget.)
# 10# forces base-10: date +%S is zero-padded ("08"/"09"), which arithmetic
# context would otherwise read as invalid octal and abort under `set -e`.
sec="10#$(date +%S)"
if [ "$((sec))" -gt 5 ]; then sleep "$((60 - sec))"; fi
CRON="$(date +'%-M %-H') * * *"
info "seeding automation: agent 'writer' + schedule trigger ('${CRON}') prompt='${MARKER}'"

cat > "${HOME}/.fleet/state.json" <<EOF
{
  "fleets": {
    "${FLEET}": {
      "name": "${FLEET}",
      "remote": "${FIXTURE_REPO_URL}",
      "settings": {
        "agents": [
          {
            "name": "writer",
            "command": "ANTHROPIC_API_KEY=itest claude --system-prompt '\${SYS_PROMPT}' '\${PROMPT}'",
            "systemPrompt": "be-terse",
            ${agent_target}
          }
        ],
        "triggers": [
          {
            "name": "fire",
            "type": "schedule",
            "agentNames": ["writer"],
            "cron": "${CRON}",
            "prompt": "${MARKER}"
          }
        ]
      },
      "instances": []
    }
  }
}
EOF

# Start the daemon (and its scheduler). The immediate startup tick fires the
# due trigger, which creates the instance.
info "starting the daemon + scheduler ('fleet ls')"
"${FLEET_BIN}" ls "${FLEET}" >/dev/null 2>&1 || true

# Wait for the scheduler to spawn the automation instance (name: writer-<HHMMSS>-<n>).
info "waiting for the scheduler to spawn an instance"
inst=""
for _ in $(seq 1 40); do
  inst=$(grep -oE 'writer-[0-9]+-[0-9]+' "${HOME}/.fleet/state.json" 2>/dev/null | head -1 || true)
  [ -n "${inst}" ] && break
  sleep 3
done
[ -n "${inst}" ] || fail "scheduler never spawned an automation instance"
info "spawned instance: ${inst}"

# Wait for the instance to provision + the agent to launch + write the marker.
# This implicitly waits for the instance to reach 'running' ('fleet exec' fails
# until then) and for launchAutomationCommand to have run the agent.
# A cluster's first create also delivers the image to the node, so the
# budget is generous.
info "waiting for the agent to launch and receive the prompt"
out=""
deadline=$(( $(date +%s) + $(_scale_timeout 600) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  out=$("${FLEET_BIN}" exec "${FLEET}/${inst}" -- cat /tmp/automation_agent_out 2>/dev/null || true)
  [ -n "${out}" ] && break
  sleep 3
done

printf 'agent output: %s\n' "${out}"
assert_contains "${out}" "AGENT_RAN"  "fake claude was never executed (agent did not launch)"
assert_contains "${out}" "${MARKER}"  "agent did not receive the trigger prompt"
assert_contains "${out}" "be-terse"   "agent did not receive the system prompt"
info "agent ran with the prompt; now waiting for the idle reaper to tear it down"

# The fake claude `exec sleep 600`s, so the agent stays alive but produces no
# screen output → the activity detector reports it idle (the Claude hook detector
# sees no state file → StateWaiting; the screen-diff fallback sees a static pane →
# StateWaiting), so after FLEET_AUTOMATION_IDLE_TIMEOUT the scheduler reaps it.
# Poll state.json until the instance record disappears (the destroy job removed
# the container + record). Budget = idle timeout + destroy time, generously. Match
# the JSON-quoted name ("writer-...") so we key off the instance's `name` field
# exactly, not a workspace-path substring that may linger.
reaped=""
for _ in $(seq 1 60); do
  if ! grep -qF -- "\"${inst}\"" "${HOME}/.fleet/state.json" 2>/dev/null; then
    reaped=1
    break
  fi
  sleep 3
done
[ -n "${reaped}" ] || fail "idle automation instance was never reaped (still present after the idle timeout)"

# The firing's event log says what became of it: the instance, then the reap.
logs=$("${FLEET_BIN}" trigger logs "${FLEET}" fire 2>&1) || fail "fleet trigger logs fire failed: ${logs}"
assert_contains "${logs}" "writer: instance ${inst} (${target_label})" "the trigger log should name the run's instance"
assert_contains "${logs}" "writer: instance ${inst} reaped after" "the trigger log should record the reap"

if ! itest_backend_is kubernetes; then
  pass "automation: fired trigger ran the agent with the prompt, then reaped the idle instance; trigger logs record both"
  exit 0
fi

# ===================== kubernetes: limits, queue, logins =====================

# free_port prints a free localhost TCP port (python3 binds :0; /dev/tcp fallback).
free_port() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
    return
  fi
  local p
  for _ in $(seq 1 100); do
    p=$(( (RANDOM % 20000) + 30000 ))
    if ! (exec 3<>"/dev/tcp/127.0.0.1/${p}") 2>/dev/null; then
      printf '%s' "${p}"
      return
    fi
    exec 3>&- 2>/dev/null || true
  done
  fail "could not find a free port"
}

# fleetd delivers webhooks through a gateway; the next daemon reads the
# setting at its start.
info "kubernetes: starting a gateway for webhook deliveries"
kill_daemons
PUBLIC_PORT="$(free_port)"
GRPC_PORT="$(free_port)"
while [ "${GRPC_PORT}" = "${PUBLIC_PORT}" ]; do GRPC_PORT="$(free_port)"; done
cfg="${HOME}/.fleet/config.json"
jq --arg gw "http://127.0.0.1:${GRPC_PORT}" '.remote_mcp_settings = {webhook_enabled: true, gateway_url: $gw}' "${cfg}" > "${cfg}.tmp"
mv "${cfg}.tmp" "${cfg}"
gw_log="${TEST_SCRATCH_DIR}/gateway.log"
"${FLEET_BIN}" gateway \
  --public-url "http://127.0.0.1:${PUBLIC_PORT}" \
  --public-addr "127.0.0.1:${PUBLIC_PORT}" \
  --grpc-addr "127.0.0.1:${GRPC_PORT}" \
  >"${gw_log}" 2>&1 &
GW_PID=$!
gw_up=false
for _ in $(seq 1 "$(_scale_timeout 50)"); do
  if curl -fsS "http://127.0.0.1:${PUBLIC_PORT}/healthz" >/dev/null 2>&1; then
    gw_up=true
    break
  fi
  kill -0 "${GW_PID}" 2>/dev/null || { cat "${gw_log}" || true; fail "gateway process exited early"; }
  sleep 0.2
done
[ "${gw_up}" = true ] || { cat "${gw_log}" || true; fail "gateway /healthz never came up"; }

# Runs of one trigger 8s apart, at most 3 of one agent at once, in pods small
# enough for three on the QA minikube. (This cold-spawns the daemon.)
INTERVAL=8
out=$("${FLEET_BIN}" cluster set itest automation_fire_interval=${INTERVAL}s automation_max_per_agent=3 \
  resources.cpu_request=100m resources.memory_request=256Mi 2>&1) || fail "fleet cluster set itest: ${out}"

out=$("${FLEET_BIN}" agent create "${FLEET}" burster --cluster itest \
  --command "ANTHROPIC_API_KEY=itest claude '\${PROMPT}'" 2>&1) || fail "agent create burster: ${out}"
out=$("${FLEET_BIN}" trigger create "${FLEET}" burst --type webhook --agent burster \
  --webhook-name itest-burst --filter-type regex --regex '.*' --prompt BURST 2>&1) || fail "trigger create burst: ${out}"

session_file="${HOME}/.fleet/gateway_session.json"
for _ in $(seq 1 "$(_scale_timeout 100)"); do
  [ -f "${session_file}" ] && grep -q '"public_webhook_url"' "${session_file}" && break
  sleep 0.2
done
webhook_base=$(grep -oE '"public_webhook_url"[[:space:]]*:[[:space:]]*"[^"]+"' "${session_file}" 2>/dev/null \
  | sed -E 's/.*"([^"]+)"$/\1/' || true)
[ -n "${webhook_base}" ] || fail "fleetd never registered a webhook URL with the gateway: $(cat "${session_file}" 2>/dev/null || true)"

# deliver <webhook> <n> — POST event n; prints the HTTP status, leaving the
# body in ${TEST_SCRATCH_DIR}/reply.<n> and the headers in .../headers.<n>.
deliver() {
  curl -sS -o "${TEST_SCRATCH_DIR}/reply.$2" -D "${TEST_SCRATCH_DIR}/headers.$2" -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' --data-binary "{\"n\":$2}" "${webhook_base}/$1" 2>/dev/null || printf '000'
}

# The first delivery is retried while the tunnel can still be coming up (an
# HTTP error fires nothing); fleetd's 200 is its verdict.
info "kubernetes: a burst of 5 deliveries to one trigger"
code=""
for _ in $(seq 1 "$(_scale_timeout 10)"); do
  code=$(deliver itest-burst 1)
  [ "${code}" = "200" ] && break
  sleep 1
done
assert_equals "200" "${code}" "the first delivery should fire at once: $(cat "${TEST_SCRATCH_DIR}/reply.1" 2>/dev/null)"
assert_contains "$(cat "${TEST_SCRATCH_DIR}/reply.1")" "received: 1 trigger(s) fired" "the first delivery's reply"
for n in 2 3 4 5; do
  code=$(deliver itest-burst "${n}")
  assert_equals "202" "${code}" "delivery ${n} should be queued: $(cat "${TEST_SCRATCH_DIR}/reply.${n}" 2>/dev/null)"
  assert_contains "$(cat "${TEST_SCRATCH_DIR}/reply.${n}")" "received: 0 fired, 1 queued" "delivery ${n}'s reply"
done

# Every event gets its run: five "instance" outcomes in the trigger's log, one
# per firing, however long the queue makes the last one wait (three run at
# once; the fourth waits for the first to be reaped).
info "kubernetes: waiting for every queued event to get its run"
started=0
deadline=$(( $(date +%s) + $(_scale_timeout 600) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  logs=$("${FLEET_BIN}" trigger logs "${FLEET}" burst 2>/dev/null || true)
  started=$(printf '%s\n' "${logs}" | grep -cE '^# outcome: [^ ]+ burster: instance burster-[0-9]+-[0-9]+ \(☸ itest\)$' || true)
  [ "${started}" -ge 5 ] && break
  sleep 5
done
printf '%s\n' "${logs}" | grep '^# outcome:' || true
assert_equals "5" "${started}" "every delivery of the burst should get its run"
events=$(printf '%s\n' "${logs}" | grep -c '^===== event-' || true)
assert_equals "5" "${events}" "the burst should be 5 firings"
queued=$(printf '%s\n' "${logs}" | grep -c 'burster: queued: ' || true)
assert_equals "4" "${queued}" "four of the burst should have waited in the queue"
refused=$(printf '%s\n' "${logs}" | grep -cE 'burster: (refused|dropped|expired|not created)' || true)
assert_equals "0" "${refused}" "no run of the burst should be lost"

# The runs started at least the fire interval apart (outcome times are whole
# seconds, so allow one second of rounding).
prev=""
while read -r stamp; do
  t=$(date -u -d "${stamp}" +%s)
  if [ -n "${prev}" ] && [ $(( t - prev )) -lt $(( INTERVAL - 1 )) ]; then
    fail "two runs of the burst started $(( t - prev ))s apart, under the ${INTERVAL}s fire interval"
  fi
  prev="${t}"
done < <(printf '%s\n' "${logs}" | sed -nE 's/^# outcome: ([^ ]+) burster: instance burster-[0-9]+-[0-9]+ \(☸ itest\)$/\1/p' | sort)

# A full queue: with an hour between runs every delivery waits, the queue
# holds 20 firings, and the next is refused with 429 and Retry-After,
# enqueueing nothing.
info "kubernetes: filling the trigger's queue"
out=$("${FLEET_BIN}" cluster set itest automation_fire_interval=1h 2>&1) || fail "fleet cluster set itest: ${out}"
for n in $(seq 101 120); do
  code=$(deliver itest-burst "${n}")
  assert_equals "202" "${code}" "delivery ${n} should be queued: $(cat "${TEST_SCRATCH_DIR}/reply.${n}" 2>/dev/null)"
done
code=$(deliver itest-burst 121)
assert_equals "429" "${code}" "a delivery to a full queue should get 429: $(cat "${TEST_SCRATCH_DIR}/reply.121" 2>/dev/null)"
grep -qi '^retry-after: [1-9]' "${TEST_SCRATCH_DIR}/headers.121" \
  || fail "the 429 should carry Retry-After: $(cat "${TEST_SCRATCH_DIR}/headers.121")"
logs=$("${FLEET_BIN}" trigger logs "${FLEET}" burst 2>&1)
events=$(printf '%s\n' "${logs}" | grep -c '^===== event-' || true)
assert_equals "25" "${events}" "the 429'd delivery should fire nothing (5 + 20 firings)"

# An agent whose tool was never logged in for this fleet on the cluster: its
# save warns, and each firing is refused with the reason.
info "kubernetes: an agent with no login on the cluster"
out=$("${FLEET_BIN}" agent create "${FLEET}" needlogin --cluster itest \
  --command "claude '\${PROMPT}'" 2>&1) || fail "agent create needlogin: ${out}"
assert_contains "${out}" "warning: agent needlogin: log in to Claude once for this fleet on ☸ itest" \
  "saving an agent with no login where it runs should warn"
out=$("${FLEET_BIN}" trigger create "${FLEET}" nologin --type webhook --agent needlogin \
  --webhook-name itest-nologin --filter-type regex --regex '.*' 2>&1) || fail "trigger create nologin: ${out}"
code=$(deliver itest-nologin 200)
assert_equals "200" "${code}" "the no-login delivery: $(cat "${TEST_SCRATCH_DIR}/reply.200" 2>/dev/null)"

# An agent whose connection was forgotten: refused with the reason.
info "kubernetes: an agent on a forgotten connection"
GONE_NS="${FLEET_ITEST_NAMESPACE}-gone"
out=$("${FLEET_BIN}" cluster add "${FLEET_ITEST_CLUSTER}" --name gone -n "${GONE_NS}" --create-namespace 2>&1) \
  || fail "fleet cluster add gone: ${out}"
gone_id=$(jq -r '.clusters[] | select(.display_name == "gone") | .id' "${HOME}/.fleet/kube_clusters.json")
[ -n "${gone_id}" ] || fail "connection gone has no id: $(cat "${HOME}/.fleet/kube_clusters.json")"
out=$("${FLEET_BIN}" agent create "${FLEET}" goner --cluster gone --command "true" 2>&1) || fail "agent create goner: ${out}"
out=$("${FLEET_BIN}" trigger create "${FLEET}" forgot --type webhook --agent goner \
  --webhook-name itest-forgot --filter-type regex --regex '.*' 2>&1) || fail "trigger create forgot: ${out}"
out=$("${FLEET_BIN}" cluster rm gone --forget 2>&1) || fail "fleet cluster rm gone --forget: ${out}"
code=$(deliver itest-forgot 300)
assert_equals "200" "${code}" "the forgotten-cluster delivery: $(cat "${TEST_SCRATCH_DIR}/reply.300" 2>/dev/null)"

# Both refusals are in their trigger logs (the firings are recorded by the
# scheduler a moment after the reply), and neither made an instance.
wait_log() {  # <trigger> <text>
  local deadline=$(( $(date +%s) + $(_scale_timeout 30) )) logs=""
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    logs=$("${FLEET_BIN}" trigger logs "${FLEET}" "$1" 2>/dev/null || true)
    case "${logs}" in *"$2"*) return 0 ;; esac
    sleep 1
  done
  printf '%s\n' "${logs}" >&2
  fail "trigger $1's log never said: $2"
}
wait_log nologin "needlogin: refused: log in to Claude once for this fleet on ☸ itest"
wait_log forgot "goner: refused: cluster ${gone_id} is no longer connected"
if grep -qE '"(needlogin|goner)-[0-9]+-[0-9]+"' "${HOME}/.fleet/state.json"; then
  fail "a refused agent got an instance: $(grep -oE '"(needlogin|goner)-[0-9]+-[0-9]+"' "${HOME}/.fleet/state.json")"
fi

pass "automation on kubernetes: the run is reaped and logged; a 5-delivery burst queues (202) and every event runs ${INTERVAL}s apart; a full queue gets 429; no-login and forgotten-cluster agents are refused with the reason in their trigger logs"
