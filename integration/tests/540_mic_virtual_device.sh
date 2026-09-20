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
selected_device="pulse:itest_desk_mic"
cat > "${HOME}/.fleet/config.json" <<JSON
{ "mic_settings": { "enabled": true, "device": "${selected_device}" } }
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
#
# Its SHAPE matters as much: it is a script file (so `sh -c` forks and the
# recorder is fleet's GRANDCHILD) and a loop that shrugs off EPIPE (head dies of
# SIGPIPE, the loop carries on). That is the recorder that is hardest to close —
# killing fleet's direct child leaves this one running with the "microphone"
# open — which is what makes the no-survivors assertion below mean something.
capture="${workdir}/capture.sh"
starts="${workdir}/capture-starts.log"
cat > "${capture}" <<STUB
#!/bin/sh
echo "started device=\${FLEET_MIC_DEVICE}" >> "${starts}"
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

# wait_for_sink <n>: block until the daemon has logged its n-th sink attach —
# the observable moment the instance's virtual microphone is up AND fed.
wait_for_sink() {
  local want="$1" deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
  local seen
  until seen=$(grep -c 'mic sink attached' "${HOME}/.fleet/fleet.log" 2>/dev/null || true); [ "${seen:-0}" -ge "${want}" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "the daemon never attached sink #${want}: $(grep -i 'mic' "${HOME}/.fleet/fleet.log" | tail -5)"
    sleep 0.5
  done
}

# record <file>: record inside the instance exactly the way Claude Code's voice
# mode does, and report "<bytes> <non-zero bytes>". 6 s, so that even a recorder
# that beats the sink's event subscription — and is only noticed by its 2 s
# safety-net poll — still captures several seconds of signal.
record() {
  "${FLEET_BIN}" exec "${target}" -- sh -c "timeout 6 arecord -f S16_LE -r 16000 -c 1 -t raw -q - > $1 2>/dev/null; true"
  "${FLEET_BIN}" exec "${target}" -- sh -c "printf '%s %s' \"\$(wc -c < $1)\" \"\$(tr -d '\\000' < $1 | wc -c)\"" | tr -s '[:space:]' ' '
}

info "wait for the daemon's sink to bring the instance's virtual microphone up"
wait_for_sink 1
"${FLEET_BIN}" exec "${target}" -- sh -c 'pactl info | grep -q "Default Source: fleetmic"' \
  || fail "the virtual microphone should be the instance's default source"
[ ! -s "${starts}" ] || fail "the microphone was opened with nothing recording: $(cat "${starts}")"

info "record inside the instance exactly the way Claude Code's voice mode does"
read -r bytes loud <<< "$(record /tmp/mic-test.raw)"
info "recorded ${bytes} bytes, ${loud} non-zero"
# SIGNAL is the load-bearing check: the noise the provider captured must be what
# the recorder hears (a dead path records zeros). 6 s is 192 kB; ask for ~2 s.
[ "${loud}" -ge 64000 ] || fail "recording is (nearly) silent (${loud} non-zero bytes): the provider's audio did not reach the instance"
# PACING is the other half: a virtual device with no clock hands a recorder
# samples as fast as it asks (the failure PulseAudio is here to prevent), which
# shows up as far MORE than 6 s of audio in 6 s — never as less.
[ "${bytes}" -le 230000 ] || fail "recorded ${bytes} bytes in 6 s: the virtual microphone is not paced in real time"

info "the provider went live for that recording, and idle again after it"
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
until [ "$(tail -n 1 "${attach_log}")" = "idle" ] && grep -q "^live -> ${target}" "${attach_log}"; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "expected 'live -> ${target}' then 'idle'; got: $(cat "${attach_log}")"
  sleep 0.5
done
assert_equals "1" "$(wc -l < "${starts}" | tr -d '[:space:]')" "the microphone should have been opened exactly once"

# `fleet mic attach` was given no --device: it must record from the device chosen
# in Settings (as an open TUI would), not silently from the system default. The
# override recorder cannot be PASSED a device, so fleet tells it the selection
# through FLEET_MIC_DEVICE — which is what makes the choice observable here.
assert_equals "started device=${selected_device}" "$(cat "${starts}")" "the provider should record from the device configured in Settings"

# recorders_alive: how many processes are still running the capture stub.
recorders_alive() { pgrep -f "${capture}" | wc -l | tr -d '[:space:]'; }

# "idle" has to MEAN the microphone is closed: opened once is half the promise,
# CLOSED when the recorder detaches is the other half.
info "no recorder process survives the provider going idle"
deadline=$(( $(date +%s) + $(_scale_timeout 15) ))
until [ "$(recorders_alive)" = "0" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the provider is idle but $(recorders_alive) recorder process(es) are still running — the microphone was never closed: $(pgrep -af "${capture}")"
  sleep 0.5
done

# A stopped container SIGKILLs the instance's sound server, leaving its FIFO and
# socket behind; the restarted instance must still come back with a WORKING
# microphone (a server that starts without its source records pure silence).
info "the virtual microphone survives an instance stop/start"
"${FLEET_BIN}" stop "${target}"
"${FLEET_BIN}" start "${target}"
wait_for_sink 2
read -r bytes loud <<< "$(record /tmp/mic-test2.raw)"
info "after restart: ${bytes} bytes, ${loud} non-zero"
[ "${loud}" -ge 64000 ] || fail "recording after a restart is (nearly) silent (${loud} non-zero bytes)"

# The microphone must close when the provider dies HOWEVER it dies — and the
# deaths that matter are the ones that run no cleanup code: SIGHUP (closing the
# terminal; fleet does not handle it) and SIGKILL. A plain `kill` (SIGTERM) is
# the one signal fleet DOES handle, so on its own it proves very little.
# provider_dies_midrecording <signal>
provider_dies_midrecording() {
  local sig="$1" log="${workdir}/attach-${1}.log" deadline
  FLEET_MIC_CAPTURE="${capture}" "${FLEET_BIN}" mic attach > "${log}" 2>&1 &
  attach_pid=$!
  deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
  until grep -q '^idle' "${log}" 2>/dev/null; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "provider (for SIG${sig}) never went idle: $(cat "${log}")"
    sleep 0.5
  done
  "${FLEET_BIN}" exec "${target}" -- sh -c 'timeout 20 arecord -f S16_LE -r 16000 -c 1 -t raw -q - > /dev/null 2>&1; true' &
  local recording=$!
  deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
  until grep -q '^live' "${log}" 2>/dev/null && [ "$(recorders_alive)" != "0" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "provider (for SIG${sig}) never went live: $(cat "${log}")"
    sleep 0.2
  done
  kill -s "${sig}" "${attach_pid}"
  wait "${attach_pid}" 2>/dev/null || true
  attach_pid=""
  deadline=$(( $(date +%s) + $(_scale_timeout 15) ))
  until [ "$(recorders_alive)" = "0" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "the provider died of SIG${sig} mid-recording and its recorder lived on — the microphone is still open: $(pgrep -af "${capture}")"
    sleep 0.5
  done
  # End the in-instance recording for real: killing the local `fleet exec` does
  # not reach the arecord inside the instance, which would otherwise hold the
  # virtual microphone's demand on until its own timeout.
  "${FLEET_BIN}" exec "${target}" -- sh -c 'pkill -x arecord 2>/dev/null; true' || true
  kill "${recording}" 2>/dev/null || true
  wait "${recording}" 2>/dev/null || true
}

info "stopping the provider (SIGTERM, while idle) leaves no recorder behind"
deadline=$(( $(date +%s) + $(_scale_timeout 15) ))
until [ "$(tail -n 1 "${attach_log}")" = "idle" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "provider never returned to idle: $(cat "${attach_log}")"
  sleep 0.5
done
kill "${attach_pid}" 2>/dev/null || true
wait "${attach_pid}" 2>/dev/null || true
attach_pid=""
[ "$(recorders_alive)" = "0" ] || fail "recorder process(es) outlived the provider: $(pgrep -af "${capture}")"

info "the terminal closing mid-recording (SIGHUP) closes the microphone"
provider_dies_midrecording HUP
info "the provider being killed outright mid-recording (SIGKILL) closes the microphone"
provider_dies_midrecording KILL

pass "virtual microphone end to end"
