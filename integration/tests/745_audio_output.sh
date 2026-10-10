#!/usr/bin/env bash
# Description: instance audio output end to end, headless: what plays into an instance's default sink reaches the selected device of an attached `fleet output attach` client, its player stops when the audio goes silent, and turning output off removes the instance's virtual sink.
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

# write_output_settings <json> — save the output settings the way a Settings
# save lands: the daemon re-reads config.json every second, and a rename means
# it never reads half a file.
write_output_settings() {
  printf '{ "output_settings": %s }\n' "$1" > "${HOME}/.fleet/config.json.new"
  mv "${HOME}/.fleet/config.json.new" "${HOME}/.fleet/config.json"
}

info "audio output on (the default), this client's speaker selected"
write_output_settings '{ "enabled": true, "client": "itest-desk", "device": "pulse:itest_speaker" }'

fleet_up alpha
inst="${FIXTURE_REPO_NAME}/alpha"

info "provisioning installed the instance's audio stack"
"${FLEET_BIN}" exec "${inst}" -- sh -c 'command -v pulseaudio && command -v pactl && command -v pacat && command -v parec' >/dev/null \
  || fail "pulseaudio, pactl, pacat and parec should be installed while audio output is on"

# Stand-ins for this client's PulseAudio tools, so playback needs no sound
# hardware: pactl lists one speaker, and paplay records its arguments and every
# byte it is asked to play.
played="${workdir}/played.raw"
player_runs="${workdir}/paplay-runs.log"
mkdir -p "${workdir}/bin"
cat > "${workdir}/bin/pactl" <<'STUB'
#!/bin/sh
if [ "$1" = list ] && [ "$2" = sinks ]; then
  printf 'Sink #0\n\tState: SUSPENDED\n\tName: itest_speaker\n\tDescription: Itest Speaker\n'
  exit 0
fi
echo "pactl stub: unsupported: $*" >&2
exit 1
STUB
cat > "${workdir}/bin/paplay" <<STUB
#!/bin/sh
echo "\$*" >> "${player_runs}"
exec cat >> "${played}"
STUB
chmod +x "${workdir}/bin/pactl" "${workdir}/bin/paplay"
: > "${played}"

info "attach as an audio output client"
attach_log="${workdir}/attach.log"
PATH="${workdir}/bin:${PATH}" FLEET_MIC_CLIENT=itest-desk "${FLEET_BIN}" output attach > "${attach_log}" 2>&1 &
attach_pid=$!
deadline=$(( $(date +%s) + $(_scale_timeout 60) ))
until grep -q 'ready — waiting for instance audio' "${attach_log}" 2>/dev/null; do
  kill -0 "${attach_pid}" 2>/dev/null || fail "fleet output attach exited: $(cat "${attach_log}")"
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the client never became the selected output: $(cat "${attach_log}")"
  sleep 0.5
done

info "the daemon lists the client and its speaker"
targets=$("${FLEET_BIN}" output targets)
assert_contains "${targets}" "* itest-desk" "the selected client"
assert_contains "${targets}" "pulse:itest_speaker" "the client's speaker"

# nonzero_played — how many bytes of non-silent audio the client has played.
nonzero_played() { tr -d '\000' < "${played}" | wc -c | tr -d '[:space:]'; }

info "play noise into the instance until the client plays it"
# Noise is the loudest possible test signal: random bytes are never silence,
# which the daemon drops. Two seconds per try, played in real time by pacat;
# the first tries may land before the daemon's source has started the
# instance's sound server and attached to it.
deadline=$(( $(date +%s) + $(_scale_timeout 180) ))
until [ "$(nonzero_played)" -ge 192000 ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the client played only $(nonzero_played) non-zero bytes: $(cat "${attach_log}"; grep -i 'audio output' "${HOME}/.fleet/fleet.log" | tail -5)"
  "${FLEET_BIN}" exec "${inst}" -- sh -c 'head -c 384000 /dev/urandom | pacat --raw --format=s16le --rate=48000 --channels=2' \
    >/dev/null 2>&1 || sleep 1
done
info "client played $(nonzero_played) non-zero bytes"
"${FLEET_BIN}" exec "${inst}" -- sh -c 'pactl info | grep -q "Default Sink: fleetoutput"' \
  || fail "the instance's default sink should be fleet's output"
assert_contains "$(cat "${attach_log}")" "playing instance audio" "the client should report playback"

info "the client played through the selected speaker"
player=$(head -n 1 "${player_runs}")
assert_contains "${player}" "--raw --format=s16le --rate=48000 --channels=2" "the player's PCM format"
assert_contains "${player}" "--device=itest_speaker" "the player should use the selected device"

info "silence stops the client's player"
deadline=$(( $(date +%s) + $(_scale_timeout 20) ))
until [ "$(tail -n 1 "${attach_log}")" = "ready — waiting for instance audio" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the player kept running after the audio stopped: $(cat "${attach_log}")"
  sleep 0.5
done

info "turning audio output off removes the instance's virtual sink"
write_output_settings '{ "enabled": false, "client": "itest-desk", "device": "pulse:itest_speaker" }'
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
until grep -q 'audio output is disabled in settings' "${attach_log}"; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the client was not told output is off: $(cat "${attach_log}")"
  sleep 0.5
done
deadline=$(( $(date +%s) + $(_scale_timeout 30) ))
until [ "$("${FLEET_BIN}" exec "${inst}" -- sh -c 'pactl list short sinks 2>/dev/null | grep -c fleetoutput; true')" = "0" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the instance's fleetoutput sink outlived output being turned off"
  sleep 1
done

pass "instance audio output"
