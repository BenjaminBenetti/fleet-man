#!/usr/bin/env bash
# itest: k8s
# Description: a microphone whose path into the instance dies without a word closes within 30s: mid-recording the instance side stops answering (kubernetes: the pod's agent, so its whole session goes silent; devcontainer: the in-instance sink), the daemon misses the sink's heartbeat, and the provider goes idle and closes the real microphone.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
workdir=$(mktemp -d)
attach_pid=""
recording_pid=""
capture=""
pod=""
itest_cleanup() {
  [ -z "${attach_pid}" ] || kill "${attach_pid}" 2>/dev/null || true
  [ -z "${recording_pid}" ] || kill "${recording_pid}" 2>/dev/null || true
  [ -z "${capture}" ] || pkill -f "${capture}" 2>/dev/null || true
  # Whatever was stopped carries on, so the teardown finds it as it left it.
  if [ -n "${pod}" ]; then
    itest_k8s_kubectl exec "${pod}" -c dev -- sh -c 'pkill -CONT -f "[p]od-agent serve"; true' >/dev/null 2>&1 || true
  fi
  rm -rf "${workdir}"
}
itest_begin

setup_test

info "enable the microphone before the instance is created"
printf '{ "mic_settings": { "enabled": true } }\n' > "${HOME}/.fleet/config.json"
itest_config_written

fleet_up alpha
target="${FIXTURE_REPO_NAME}/alpha"

# A stand-in for the real microphone that logs each start (540's shape).
capture="${workdir}/capture.sh"
cat > "${capture}" <<STUB
#!/bin/sh
echo started >> "${workdir}/starts.log"
while :; do head -c 3200 /dev/urandom; sleep 0.1; done
STUB
chmod +x "${capture}"
recorders_alive() { pgrep -f "${capture}" | wc -l | tr -d '[:space:]'; }

info "attach as the microphone provider"
attach_log="${workdir}/attach.log"
FLEET_MIC_CAPTURE="${capture}" "${FLEET_BIN}" mic attach > "${attach_log}" 2>&1 &
attach_pid=$!
deadline=$(( $(date +%s) + $(_scale_timeout 180) ))
until grep -q 'mic sink attached' "${HOME}/.fleet/fleet.log" 2>/dev/null && [ "$(tail -n 1 "${attach_log}")" = "idle" ]; do
  kill -0 "${attach_pid}" 2>/dev/null || fail "fleet mic attach exited: $(cat "${attach_log}")"
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the sink never attached: $(grep -i 'mic' "${HOME}/.fleet/fleet.log" | tail -5)"
  sleep 0.5
done

info "record inside the instance: the provider goes live"
"${FLEET_BIN}" exec "${target}" -- sh -c 'timeout 300 arecord -f S16_LE -r 16000 -c 1 -t raw -q - > /dev/null 2>&1; true' >/dev/null 2>&1 &
recording_pid=$!
deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
until grep -q "^live -> ${target}" "${attach_log}" && [ "$(recorders_alive)" != "0" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the provider never went live: $(cat "${attach_log}")"
  sleep 0.2
done

if itest_backend_is kubernetes; then
  info "the pod's agent stops answering: its session goes silent"
  cid=$(itest_container_id alpha)
  pod=$(itest_k8s_pod "${cid}")
  # [p]: the pattern must not match the sh running it.
  itest_k8s_kubectl exec "${pod}" -c dev -- sh -c 'pkill -STOP -f "[p]od-agent serve"' \
    || fail "could not stop the pod's agent"
else
  info "the in-instance sink stops answering"
  "${FLEET_BIN}" exec "${target}" -- sh -c 'pkill -STOP -f "[f]leet mic sink"' \
    || fail "could not stop the sink"
fi
dropped=$(date +%s)

# The heartbeat deadline is 15s; the promise is 30s.
deadline=$(( dropped + 30 ))
until [ "$(tail -n 1 "${attach_log}")" = "idle" ] && [ "$(recorders_alive)" = "0" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "30s after the drop the microphone is still open: $(tail -n 3 "${attach_log}"); $(recorders_alive) recorder(s)"
  sleep 0.5
done
info "the microphone closed $(( $(date +%s) - dropped ))s after the drop"
grep -q 'missed its heartbeat\|mic sink lost' "${HOME}/.fleet/fleet.log" \
  || fail "the daemon did not say why the sink went: $(grep -i 'mic' "${HOME}/.fleet/fleet.log" | tail -5)"

pass "a microphone on a dead path closes within 30s"
