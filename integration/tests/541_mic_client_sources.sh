#!/usr/bin/env bash
# Description: with two microphone clients attached, the one selected in Settings is the one recorded — not whichever attached last — the selection moves the microphone between them, and a selected client that is not attached is stood in for.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
workdir=$(mktemp -d)
desk_pid=""
laptop_pid=""
itest_cleanup() {
  [ -z "${desk_pid}" ] || kill "${desk_pid}" 2>/dev/null || true
  [ -z "${laptop_pid}" ] || kill "${laptop_pid}" 2>/dev/null || true
  # A failed assertion can exit with a capture stub still running.
  pkill -f "${workdir}/capture-" 2>/dev/null || true
  rm -rf "${workdir}"
}
itest_begin

setup_test

# select_mic <client> <device>: save the microphone selection. The settings
# page does this through SetConfig; a config.json edit reaches the daemon the
# same way a hand edit does — its sync loop reads the selection every second —
# and written with a rename so the daemon never reads half a file.
select_mic() {
  cat > "${HOME}/.fleet/config.json.new" <<JSON
{ "mic_settings": { "enabled": true, "client": "$1", "device": "$2" } }
JSON
  mv "${HOME}/.fleet/config.json.new" "${HOME}/.fleet/config.json"
}

info "enable the microphone (no client selected) before the instance is created"
select_mic "" ""

fleet_up alpha
target="${FIXTURE_REPO_NAME}/alpha"

# One stand-in microphone per client: each logs every start (with the device it
# was asked for) to its OWN file, which is what makes "whose microphone was
# opened" observable.
make_capture() {
  local name="$1" script="${workdir}/capture-$1.sh"
  cat > "${script}" <<STUB
#!/bin/sh
echo "started device=\${FLEET_MIC_DEVICE}" >> "${workdir}/starts-${name}.log"
while :; do head -c 3200 /dev/urandom; sleep 0.1; done
STUB
  chmod +x "${script}"
  : > "${workdir}/starts-${name}.log"
  printf '%s' "${script}"
}
starts() { wc -l < "${workdir}/starts-$1.log" | tr -d '[:space:]'; }

# attach <client>: provide a microphone as the machine <client>; echoes the pid.
attach() {
  FLEET_MIC_CLIENT="$1" FLEET_MIC_CAPTURE="$(make_capture "$1")" \
    "${FLEET_BIN}" mic attach > "${workdir}/attach-$1.log" 2>&1 &
  echo $!
}
wait_idle() {
  local deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
  until grep -q '^idle' "${workdir}/attach-$1.log" 2>/dev/null; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "provider $1 never went idle: $(cat "${workdir}/attach-$1.log")"
    sleep 0.5
  done
}

info "attach two microphone clients: desk first, then laptop"
desk_pid=$(attach desk)
wait_idle desk
laptop_pid=$(attach laptop)
wait_idle laptop

# wait_source <client>: block until `fleet mic sources` marks <client> as the
# one being recorded.
wait_source() {
  local deadline=$(( $(date +%s) + $(_scale_timeout 30) )) listing
  until listing=$("${FLEET_BIN}" mic sources 2>&1) && printf '%s\n' "${listing}" | grep -Eq "^\* $1 "; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "'fleet mic sources' never marked $1 as the source: ${listing}"
    sleep 0.5
  done
}

info "both clients are listed, and with nothing selected the newest one is the source"
wait_source laptop
listing=$("${FLEET_BIN}" mic sources)
assert_contains "${listing}" "desk" "'fleet mic sources' should list every attached client"
assert_contains "${listing}" "(default)" "each client offers at least its system default"

info "wait for the daemon's sink to bring the instance's virtual microphone up"
deadline=$(( $(date +%s) + $(_scale_timeout 120) ))
until grep -q 'mic sink attached' "${HOME}/.fleet/fleet.log" 2>/dev/null; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the daemon never attached a sink: $(grep -i 'mic' "${HOME}/.fleet/fleet.log" | tail -5)"
  sleep 0.5
done

# record: record inside the instance the way Claude Code's voice mode does and
# report the number of non-zero bytes heard.
record() {
  "${FLEET_BIN}" exec "${target}" -- sh -c 'timeout 5 arecord -f S16_LE -r 16000 -c 1 -t raw -q - > /tmp/mic-src.raw 2>/dev/null; true'
  "${FLEET_BIN}" exec "${target}" -- sh -c "tr -d '\\000' < /tmp/mic-src.raw | wc -c" | tr -d '[:space:]'
}
# all_closed: every provider is idle again and no capture stub is running.
all_closed() {
  local deadline=$(( $(date +%s) + $(_scale_timeout 20) ))
  until [ "$(pgrep -f "${workdir}/capture-" | wc -l | tr -d '[:space:]')" = "0" ]; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "a microphone is still open after the recording ended: $(pgrep -af "${workdir}/capture-")"
    sleep 0.5
  done
}

info "nothing selected: the recording comes from laptop, and desk's microphone stays closed"
loud=$(record)
[ "${loud}" -ge 32000 ] || fail "recording is (nearly) silent (${loud} non-zero bytes)"
all_closed
assert_equals "1" "$(starts laptop)" "laptop (the newest client) should have been recorded"
assert_equals "0" "$(starts desk)" "desk's microphone must stay closed while laptop is the source"

info "select desk: the microphone moves to it although laptop attached later"
select_mic desk "pulse:desk_usb"
wait_source desk
loud=$(record)
[ "${loud}" -ge 32000 ] || fail "recording from the selected client is (nearly) silent (${loud} non-zero bytes)"
all_closed
assert_equals "1" "$(starts desk)" "the selected client's microphone should have been opened"
assert_equals "1" "$(starts laptop)" "laptop's microphone must not be opened once desk is selected"
assert_equals "started device=pulse:desk_usb" "$(cat "${workdir}/starts-desk.log")" "the selected client should record from the selected device"
grep -q "^live -> ${target}" "${workdir}/attach-desk.log" || fail "desk never reported going live: $(cat "${workdir}/attach-desk.log")"

info "select a client that is not attached: the newest attached client stands in, on its own default"
select_mic tablet "pulse:tablet_mic"
wait_source laptop
loud=$(record)
[ "${loud}" -ge 32000 ] || fail "the stand-in's recording is (nearly) silent (${loud} non-zero bytes)"
all_closed
assert_equals "2" "$(starts laptop)" "laptop should stand in for the absent client"
assert_equals "1" "$(starts desk)" "desk is not the newest client and must not be opened"
assert_equals "started device=" "$(tail -n 1 "${workdir}/starts-laptop.log")" "a stand-in records its system default — the selected device belongs to the other machine"
grep -q "standing in for tablet" "${workdir}/attach-laptop.log" || fail "laptop should say whom it stands in for: $(cat "${workdir}/attach-laptop.log")"

info "the source leaving hands the microphone to the next client"
kill "${laptop_pid}" 2>/dev/null || true
wait "${laptop_pid}" 2>/dev/null || true
laptop_pid=""
wait_source desk
listing=$("${FLEET_BIN}" mic sources)
if printf '%s\n' "${listing}" | grep -q "laptop"; then
  fail "a client that left is still listed: ${listing}"
fi

pass "microphone source selection across clients"
