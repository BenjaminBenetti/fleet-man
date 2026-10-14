#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster, a pod's bulk session is paced only while audio actually flows on it: a 48MB fleet copy into an instance whose audio output is on but silent runs unpaced, pacing engages while a tone plays (every other PCM frame counts as late here: FLEET_AUDIO_LATE_THRESHOLD), and once the tone ends the next copy is unpaced again.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
workdir=$(mktemp -d)
attach_pid=""
tone_pid=""
itest_cleanup() {
  [ -z "${tone_pid}" ] || kill "${tone_pid}" 2>/dev/null || true
  [ -z "${attach_pid}" ] || kill "${attach_pid}" 2>/dev/null || true
  rm -rf "${workdir}"
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: a pod's bulk session paced against its audio"

setup_test

# On a local cluster a copy never crowds the PCM out by 60ms; at 1us about
# every other frame of an audible stream reaches fleetd late, so the pacer
# keeps cutting the rate for as long as the tone plays, and what this test
# sees is whether the bulk session is paced exactly while audio flows. The
# daemon setup started is restarted by itest_config_written, with the
# threshold.
export FLEET_AUDIO_LATE_THRESHOLD=1us
info "audio output on, this client's speaker selected"
printf '{ "output_settings": { "enabled": true, "client": "itest-desk", "device": "pulse:itest_speaker" } }\n' > "${HOME}/.fleet/config.json.new"
mv "${HOME}/.fleet/config.json.new" "${HOME}/.fleet/config.json"
itest_config_written

fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

# The client's PulseAudio tools, as 745 stands them in: one speaker, and a
# player that records every byte it plays.
played="${workdir}/played.raw"
mkdir -p "${workdir}/bin"
cat > "${workdir}/bin/pactl" <<'STUB'
#!/bin/sh
if [ "$1" = list ] && [ "$2" = sinks ]; then
  printf 'Sink #0\n\tState: SUSPENDED\n\tName: itest_speaker\n\tDescription: Itest Speaker\n'
  exit 0
fi
exit 1
STUB
cat > "${workdir}/bin/paplay" <<STUB
#!/bin/sh
exec cat >> "${played}"
STUB
chmod +x "${workdir}/bin/pactl" "${workdir}/bin/paplay"
: > "${played}"
nonzero_played() { tr -d '\000' < "${played}" | wc -c | tr -d '[:space:]'; }

info "attach as the audio output client"
attach_log="${workdir}/attach.log"
PATH="${workdir}/bin:${PATH}" FLEET_MIC_CLIENT=itest-desk "${FLEET_BIN}" output attach > "${attach_log}" 2>&1 &
attach_pid=$!
deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
until grep -q 'ready — waiting for instance audio' "${attach_log}" 2>/dev/null; do
  kill -0 "${attach_pid}" 2>/dev/null || fail "fleet output attach exited: $(cat "${attach_log}")"
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the client never became the selected output: $(cat "${attach_log}")"
  sleep 0.5
done
deadline=$(( $(date +%s) + $(_scale_timeout 180) ))
until grep -q "audio output source attached.*${inst}" "${HOME}/.fleet/fleet.log" 2>/dev/null; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the instance's output source never attached: $(grep -i 'audio output' "${HOME}/.fleet/fleet.log" | tail -5)"
  sleep 0.5
done

paced() { grep -c 'bulk session paced while audio flows' "${HOME}/.fleet/fleet.log" 2>/dev/null || true; }
unpaced() { grep -c 'bulk session unpaced' "${HOME}/.fleet/fleet.log" 2>/dev/null || true; }

# copy48 <name> — copy 48MB into the instance over its bulk session, check it
# arrived whole, and print how long it took (ms).
head -c 50331648 /dev/urandom > "${workdir}/big.bin"
want_sum=$(sha256sum "${workdir}/big.bin" | cut -d' ' -f1)
copy48() {
  local start end got
  start=$(date +%s%N)
  "${FLEET_BIN}" copy "${workdir}/big.bin" "${inst}:/tmp/$1" >/dev/null
  end=$(date +%s%N)
  got=$("${FLEET_BIN}" exec "${inst}" -- sha256sum "/tmp/$1" | cut -d' ' -f1)
  [ "${got}" = "${want_sum}" ] || fail "the copy /tmp/$1 arrived as ${got}, want ${want_sum}"
  "${FLEET_BIN}" exec "${inst}" -- rm -f "/tmp/$1"
  echo $(( (end - start) / 1000000 ))
}

info "a 48MB copy while the output is on but silent: unpaced"
before=$(paced)
silent_ms=$(copy48 silent.bin)
[ "$(paced)" = "${before}" ] || fail "the bulk session was paced while nothing played: $(grep 'bulk session' "${HOME}/.fleet/fleet.log" | tail -3)"
[ "$(nonzero_played)" = "0" ] || fail "the client played $(nonzero_played) bytes with nothing playing in the instance"
info "silent: ${silent_ms}ms"

info "play noise into the instance until the client hears it"
# 60s of noise (never silence), played in real time.
"${FLEET_BIN}" exec "${inst}" -- sh -c 'head -c 11520000 /dev/urandom | pacat --raw --format=s16le --rate=48000 --channels=2' >/dev/null 2>&1 &
tone_pid=$!
deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
until [ "$(nonzero_played)" -ge 96000 ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the client heard only $(nonzero_played) bytes of the tone: $(cat "${attach_log}")"
  sleep 0.5
done

info "pacing engages while the tone plays"
# With the tone's first late frame, before any copy: audio flows on the pod.
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
until [ "$(paced)" -ge 1 ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the bulk session was not paced while audio flowed: $(grep -E 'bulk session|audio output' "${HOME}/.fleet/fleet.log")"
  sleep 0.5
done
grep 'bulk session paced while audio flows' "${HOME}/.fleet/fleet.log" | tail -n 1 | grep -q "instance=alpha" \
  || fail "the pacing line does not name the instance: $(grep 'bulk session paced' "${HOME}/.fleet/fleet.log" | tail -1)"
# The copy runs at the paced rate while frames keep arriving late, and the
# rate recovers while they arrive on time: how long it takes depends on
# that, so it is reported, not judged. The tone keeps reaching the client.
heard_before=$(nonzero_played)
tone_ms=$(copy48 tone.bin)
info "while the tone played: ${tone_ms}ms; the client heard $(( $(nonzero_played) - heard_before )) bytes meanwhile"
grep -E 'bulk session' "${HOME}/.fleet/fleet.log" | sed 's/^/    /' >&2
[ "$(( $(nonzero_played) - heard_before ))" -ge 96000 ] || fail "the tone did not reach the client during the copy"

info "the tone ends: the next copy is unpaced again"
"${FLEET_BIN}" exec "${inst}" -- sh -c 'pkill -x pacat; true' >/dev/null 2>&1 || true
kill "${tone_pid}" 2>/dev/null || true
wait "${tone_pid}" 2>/dev/null || true
tone_pid=""
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
until [ "$(tail -n 1 "${attach_log}")" = "ready — waiting for instance audio" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the client kept playing after the tone ended: $(tail -3 "${attach_log}")"
  sleep 0.5
done
sleep 3 # past the 2s an audible frame keeps audio flowing
before=$(paced)
after_ms=$(copy48 after.bin)
[ "$(paced)" = "${before}" ] || fail "the bulk session was paced after the tone ended: $(grep 'bulk session' "${HOME}/.fleet/fleet.log" | tail -3)"
[ "$(unpaced)" -ge 1 ] || fail "pacing never ended: $(grep 'bulk session' "${HOME}/.fleet/fleet.log" | tail -3)"
info "after the tone: ${after_ms}ms"

pass "a pod's bulk session is paced only while audio flows"
