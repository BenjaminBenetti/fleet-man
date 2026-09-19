#!/usr/bin/env bash
# Description: with the microphone enabled, provisioning installs the audio stack and a recorder inside the instance hears what `fleet mic attach` captures — only while it records, and again after an instance restart.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
workdir=$(mktemp -d)
attach_pid=""
itest_cleanup() {
  [ -z "${attach_pid}" ] || kill "${attach_pid}" 2>/dev/null || true
  rm -rf "${workdir}"
}
itest_begin

setup_test

info "enable the microphone before the instance is created"
cat > "${HOME}/.fleet/config.json" <<JSON
{ "mic_settings": { "enabled": true } }
JSON

fleet_up alpha
target="${FIXTURE_REPO_NAME}/alpha"

info "provisioning installed the audio stack and made the virtual mic the ALSA default"
"${FLEET_BIN}" exec "${target}" -- sh -c 'command -v pulseaudio && command -v pactl && command -v arecord' >/dev/null \
  || fail "pulseaudio / pactl / arecord should be installed when the microphone is enabled"
asound=$("${FLEET_BIN}" exec "${target}" -- cat /etc/asound.conf)
assert_contains "${asound}" "type pulse" "/etc/asound.conf should route the ALSA default to pulse"
assert_contains "${asound}" "managed by fleet" "/etc/asound.conf should carry fleet's marker"

# A stand-in for the real microphone: a loud, paced, never-ending signal. It
# logs each start so the test can prove capture is ON DEMAND — the real
# microphone must stay closed until something in an instance records.
capture="${workdir}/capture.sh"
starts="${workdir}/capture-starts.log"
cat > "${capture}" <<STUB
#!/bin/sh
echo started >> "${starts}"
while :; do head -c 3200 /dev/urandom; sleep 0.1; done
STUB
chmod +x "${capture}"
: > "${starts}"

info "attach as the microphone provider"
attach_log="${workdir}/attach.log"
FLEET_MIC_CAPTURE="${capture}" "${FLEET_BIN}" mic attach > "${attach_log}" 2>&1 &
attach_pid=$!

deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
until grep -q '^idle' "${attach_log}" 2>/dev/null; do
  kill -0 "${attach_pid}" 2>/dev/null || fail "fleet mic attach exited early: $(cat "${attach_log}")"
  [ "$(date +%s)" -lt "${deadline}" ] || fail "provider never went idle: $(cat "${attach_log}")"
  sleep 0.5
done

info "wait for the daemon's sink to bring the instance's virtual microphone up"
deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
until "${FLEET_BIN}" exec "${target}" -- sh -c 'pactl info 2>/dev/null | grep -q "Default Source: fleetmic"'; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the instance never got its virtual microphone"
  sleep 1
done
# Let the sink settle into its subscribe loop before the first recording.
sleep 2
[ ! -s "${starts}" ] || fail "the microphone was opened with nothing recording: $(cat "${starts}")"

info "record inside the instance exactly the way Claude Code's voice mode does"
"${FLEET_BIN}" exec "${target}" -- sh -c 'timeout 4 arecord -f S16_LE -r 16000 -c 1 -t raw -q - > /tmp/mic-test.raw; true'
bytes=$("${FLEET_BIN}" exec "${target}" -- sh -c 'wc -c < /tmp/mic-test.raw' | tr -d '[:space:]')
loud=$("${FLEET_BIN}" exec "${target}" -- sh -c "tr -d '\\000' < /tmp/mic-test.raw | wc -c" | tr -d '[:space:]')
info "recorded ${bytes} bytes, ${loud} non-zero"
# 4 s at 32 kB/s is 128 kB; demand signalling and capture start-up eat into the
# front of it, so ask for a bit over a second of real signal.
[ "${bytes}" -ge 40000 ] || fail "recording too short (${bytes} bytes): the virtual microphone is not pacing/delivering audio"
[ "${loud}" -ge 30000 ] || fail "recording is (nearly) silent (${loud} non-zero bytes): the provider's audio did not reach the instance"

info "the provider went live for that recording, and idle again after it"
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
until [ "$(tail -n 1 "${attach_log}")" = "idle" ] && grep -q "^live -> ${target}" "${attach_log}"; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "expected 'live -> ${target}' then 'idle'; got: $(cat "${attach_log}")"
  sleep 0.5
done
assert_equals "1" "$(wc -l < "${starts}" | tr -d '[:space:]')" "the microphone should have been opened exactly once"

# A stopped container SIGKILLs the instance's sound server, leaving its FIFO and
# socket behind; the restarted instance must still come back with a WORKING
# microphone (a server that starts without its source records pure silence).
info "the virtual microphone survives an instance stop/start"
"${FLEET_BIN}" stop "${target}"
"${FLEET_BIN}" start "${target}"
deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
until "${FLEET_BIN}" exec "${target}" -- sh -c 'pactl info 2>/dev/null | grep -q "Default Source: fleetmic"'; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the virtual microphone did not come back after a restart"
  sleep 1
done
sleep 2
"${FLEET_BIN}" exec "${target}" -- sh -c 'timeout 4 arecord -f S16_LE -r 16000 -c 1 -t raw -q - > /tmp/mic-test2.raw; true'
loud=$("${FLEET_BIN}" exec "${target}" -- sh -c "tr -d '\\000' < /tmp/mic-test2.raw | wc -c" | tr -d '[:space:]')
info "after restart: ${loud} non-zero bytes"
[ "${loud}" -ge 30000 ] || fail "recording after a restart is (nearly) silent (${loud} non-zero bytes)"

pass "virtual microphone end to end"
