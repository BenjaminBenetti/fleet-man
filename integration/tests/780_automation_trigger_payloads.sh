#!/usr/bin/env bash
# itest: k8s
# Description: automation trigger payload files (391 variant) — a webhook event delivered through the gateway and a bash trigger's stdout each spawn an agent instance (on the run's cluster on kubernetes, where the file goes through the pod agent's FileWrite); the payload lands byte-exact in /tmp/fleet-event-<ts> inside it, the agent's prompt names that file, and fleet trigger logs records the firing.
set -euo pipefail

source "$(dirname "$0")/../common.sh"

GW_PID=""
# Kill our daemon (short scheduler timings in its env) and the gateway on exit.
itest_cleanup() {
  pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
  if [ -n "${GW_PID}" ]; then
    kill "${GW_PID}" 2>/dev/null || true
    wait "${GW_PID}" 2>/dev/null || true
  fi
}
itest_begin

# A 5s tick launches a spawned agent promptly once its instance runs. The idle
# timeout only has to outlast the checks below (the reap itself is 391's).
export FLEET_AUTOMATION_IDLE_TIMEOUT="10m"
export FLEET_AUTOMATION_TICK_INTERVAL="5s"

setup_automation_test

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

# A lingering daemon from an earlier test has neither our env nor the webhook
# config; kill it so the first CLI call below cold-spawns a fresh one.
info "killing any lingering daemon so we cold-spawn one with our env"
pkill -f "${FLEET_BIN} server" >/dev/null 2>&1 || true
deadline=$(( $(date +%s) + $(_scale_timeout 5) ))
while [ "$(date +%s)" -lt "${deadline}" ]; do
  [ "$(server_count)" = "0" ] && break
  sleep 0.2
done

FLEET="${FIXTURE_REPO_NAME}"

# --- gateway: fleetd delivers webhooks through it ---
PUBLIC_PORT="$(free_port)"
GRPC_PORT="$(free_port)"
while [ "${GRPC_PORT}" = "${PUBLIC_PORT}" ]; do GRPC_PORT="$(free_port)"; done
cat > "${HOME}/.fleet/config.json" <<EOF
{
  "remote_mcp_settings": {
    "webhook_enabled": true,
    "gateway_url": "http://127.0.0.1:${GRPC_PORT}"
  }
}
EOF
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

# --- the payloads ---
# The webhook body carries quotes, a $(...), backticks, a backslash, UTF-8 and
# no trailing newline: it must reach the instance byte for byte, never through
# a shell.
webhook_body="${TEST_SCRATCH_DIR}/webhook-body.json"
printf '%s' '{"action":"opened","marker":"WEBHOOK_PAYLOAD_780","text":"it'"'"'s \"quoted\" $(not run) `nor this` C:\\path h\u00e9llo ✓"}' \
  > "${webhook_body}"
# The bash trigger runs on the daemon host each minute; the guard file makes it
# exit 0 (fire) exactly once. Its stdout is the payload.
bash_guard="${TEST_SCRATCH_DIR}/bash-trigger-fired"
bash_expected="${TEST_SCRATCH_DIR}/bash-payload.expected"
printf 'BASH_PAYLOAD_780\nsecond line: $HOME stays literal\n' > "${bash_expected}"
bash_script="[ -e '${bash_guard}' ] && exit 1; : > '${bash_guard}'; printf '%s\n' BASH_PAYLOAD_780 'second line: \$HOME stays literal'"

# --- automation config, through the CLI (this spawns the daemon) ---
cat > "${HOME}/.fleet/state.json" <<EOF
{
  "fleets": {
    "${FLEET}": {
      "name": "${FLEET}",
      "remote": "${FIXTURE_REPO_URL}",
      "settings": {},
      "instances": []
    }
  }
}
EOF
info "creating agents and triggers (fleet agent / fleet trigger create)"
# An agent's target is stored concretely: the run's connection on
# kubernetes (named as `fleet cluster` names it), devcontainer otherwise.
agent_target=(--backend devcontainer)
if itest_backend_is kubernetes; then
  agent_target=(--cluster itest)
fi
for agent in hooker basher; do
  out=$("${FLEET_BIN}" agent create "${FLEET}" "${agent}" "${agent_target[@]}" \
    --command "claude --system-prompt '\${SYS_PROMPT}' '\${PROMPT}'" --system-prompt "be-terse" 2>&1) \
    || fail "agent create ${agent} failed: ${out}"
done
out=$("${FLEET_BIN}" trigger create "${FLEET}" hook --type webhook --agent hooker \
  --webhook-name itest-hook --filter-type jsonpath --json-path '$.action' --json-value opened \
  --prompt "WEBHOOK_PROMPT_780" 2>&1) || fail "webhook trigger create failed: ${out}"

# --- webhook delivery through the gateway ---
session_file="${HOME}/.fleet/gateway_session.json"
for _ in $(seq 1 "$(_scale_timeout 100)"); do
  [ -f "${session_file}" ] && grep -q '"public_webhook_url"' "${session_file}" && break
  sleep 0.2
done
webhook_base=$(grep -oE '"public_webhook_url"[[:space:]]*:[[:space:]]*"[^"]+"' "${session_file}" 2>/dev/null \
  | sed -E 's/.*"([^"]+)"$/\1/' || true)
[ -n "${webhook_base}" ] || fail "fleetd never registered a webhook URL with the gateway: $(cat "${session_file}" 2>/dev/null || true)"
info "webhook URL: ${webhook_base}/itest-hook"

# Retried while the reply is an HTTP error, which fires nothing (the tunnel can
# still be coming up right after registration); a 2xx is fleetd's verdict.
info "POSTing the webhook event"
reply=""
for _ in $(seq 1 "$(_scale_timeout 10)"); do
  reply=$(curl -sS -f -X POST -H 'Content-Type: application/json' --data-binary "@${webhook_body}" \
    "${webhook_base}/itest-hook" 2>&1) && break
  reply="failed: ${reply}"
  sleep 1
done
assert_contains "${reply}" "1 trigger(s) fired" "the webhook should fire the hook trigger"

info "creating the bash trigger (fires on the next scheduler tick)"
out=$("${FLEET_BIN}" trigger create "${FLEET}" probe --type bash --agent basher --cron '* * * * *' \
  --script "${bash_script}" --prompt "BASH_PROMPT_780" 2>&1) || fail "bash trigger create failed: ${out}"

# wait_instance <agent> — the instance the scheduler spawned for <agent>
# (<agent>-<HHMMSS>-<n>).
wait_instance() {
  local name="" deadline
  deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    name=$(grep -oE "\"$1-[0-9]+-[0-9]+\"" "${HOME}/.fleet/state.json" 2>/dev/null | head -n 1 | tr -d '"' || true)
    [ -n "${name}" ] && { printf '%s\n' "${name}"; return 0; }
    sleep 2
  done
  return 1
}
hook_inst=$(wait_instance hooker) || fail "the webhook never spawned an agent instance"
bash_inst=$(wait_instance basher) || fail "the bash trigger never spawned an agent instance"
info "spawned: ${hook_inst} (webhook), ${bash_inst} (bash)"
for inst in "${hook_inst}" "${bash_inst}"; do
  assert_equals "${FLEET_ITEST_BACKEND}" \
    "$(jq -r --arg f "${FLEET}" --arg i "${inst}" '.fleets[$f].instances[] | select(.name == $i) | .backend' "${HOME}/.fleet/state.json")" \
    "${inst} should run on its agent's target"
done

# agent_output <instance> — what the fake claude recorded once it launched.
agent_output() {
  local out="" deadline
  deadline=$(( $(date +%s) + $(_scale_timeout 300) ))
  while [ "$(date +%s)" -lt "${deadline}" ]; do
    out=$("${FLEET_BIN}" exec "${FLEET}/$1" -- cat /tmp/automation_agent_out 2>/dev/null || true)
    [ -n "${out}" ] && { printf '%s\n' "${out}"; return 0; }
    sleep 3
  done
  return 1
}

# check_payload <instance> <kind> <prompt> <expected-file> — the agent ran with
# the trigger's prompt plus a note naming its event file, and that file holds
# exactly the expected payload.
check_payload() {
  local inst="$1" kind="$2" prompt="$3" expected="$4" out path got
  out=$(agent_output "${inst}") || fail "the ${kind} agent in ${inst} never launched"
  printf '%s agent: %s\n' "${kind}" "${out}"
  assert_contains "${out}" "AGENT_RAN" "the ${kind} agent was never executed"
  assert_contains "${out}" "${prompt}" "the ${kind} agent did not receive the trigger prompt"
  assert_contains "${out}" "${kind} trigger" "the ${kind} agent's prompt should say what fired it"
  path=$(printf '%s' "${out}" | grep -oE '/tmp/fleet-event-[0-9]{8}T[0-9]{6}Z' | head -n 1 || true)
  [ -n "${path}" ] || fail "the ${kind} agent's prompt names no event file: ${out}"
  got="${TEST_SCRATCH_DIR}/${kind}-payload.got"
  "${FLEET_BIN}" exec "${FLEET}/${inst}" -- cat "${path}" > "${got}" \
    || fail "the ${kind} event file ${path} is missing in ${inst}"
  if ! cmp -s "${expected}" "${got}"; then
    printf -- '--- want ---\n%s\n--- got ---\n%s\n--- end ---\n' "$(od -c "${expected}")" "$(od -c "${got}")" >&2
    fail "the ${kind} event file ${path} does not hold the payload byte for byte"
  fi
  info "${kind} payload at ${path} matches"
}

check_payload "${hook_inst}" webhook "WEBHOOK_PROMPT_780" "${webhook_body}"
check_payload "${bash_inst}" bash "BASH_PROMPT_780" "${bash_expected}"

# --- trigger logs: each firing recorded once, with its payload ---
logs=$("${FLEET_BIN}" trigger logs "${FLEET}" hook 2>&1) || fail "fleet trigger logs hook failed: ${logs}"
assert_contains "${logs}" "# type:    webhook" "the webhook firing should be logged"
assert_contains "${logs}" "# webhook: itest-hook" "the webhook log should name the webhook"
assert_contains "${logs}" "WEBHOOK_PAYLOAD_780" "the webhook log should hold the body"
logs=$("${FLEET_BIN}" trigger logs "${FLEET}" probe 2>&1) || fail "fleet trigger logs probe failed: ${logs}"
assert_contains "${logs}" "# type:    bash" "the bash firing should be logged"
assert_contains "${logs}" "second line: \$HOME stays literal" "the bash log should hold the probe's stdout"
fired=$(printf '%s\n' "${logs}" | grep -c '^===== event-' || true)
assert_equals "1" "${fired}" "the guarded bash trigger should fire exactly once"

pass "webhook and bash trigger payloads reach the agent's instance byte-exact, named in its prompt and logged"
