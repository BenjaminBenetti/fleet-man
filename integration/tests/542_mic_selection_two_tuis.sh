#!/usr/bin/env bash
# itest: no-docker
# Description: a microphone source selected in one TUI shows up in another TUI that was already open, and that TUI saving an unrelated setting does not revert it.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
workdir=$(mktemp -d)
laptop="fleetman-itest-mic-laptop"
desk="fleetman-itest-mic-desk"
itest_cleanup() {
  tui_kill "${laptop}"
  tui_kill "${desk}"
  pkill -f "${workdir}/capture.sh" 2>/dev/null || true
  rm -rf "${workdir}"
}
itest_begin

setup_test

info "enable the microphone, nothing selected"
mkdir -p "${HOME}/.fleet"
cat > "${HOME}/.fleet/config.json" <<JSON
{ "mic_settings": { "enabled": true } }
JSON

# A stand-in recorder, so each TUI counts as a machine that can record and
# attaches as a microphone client. It is never started: nothing records here.
capture="${workdir}/capture.sh"
cat > "${capture}" <<'STUB'
#!/bin/sh
while :; do head -c 3200 /dev/zero; sleep 0.1; done
STUB
chmod +x "${capture}"

# spawn_tui <session> <client>: a TUI that is the machine <client> to the
# daemon. The variables go on the command line because a tmux session takes its
# environment from the tmux server, not from this shell.
spawn_tui() {
  tmux kill-session -t "$1" >/dev/null 2>&1 || true
  tmux new-session -d -s "$1" -x "${TUI_WIDTH}" -y "${TUI_HEIGHT}" \
    "env TERM=xterm-256color FLEET_MIC_CLIENT=$2 FLEET_MIC_CAPTURE=${capture} ${FLEET_BIN}"
  tui_wait_for "No instances" 20 "$1"
  # With no fleets the cursor sits on the settings row.
  tui_send Enter "$1"
  tui_wait_for "Tmux vim keys" 10 "$1"
}

info "open a TUI as laptop, then one as desk — both on the settings page"
spawn_tui "${laptop}" laptop
spawn_tui "${desk}" desk

# shows <session> <text>: the session's screen contains <text> right now.
shows() { tui_capture "$1" | grep -qF -- "$2"; }

# wait_source <client>: `fleet mic sources` marks <client> as the one recorded.
wait_source() {
  local deadline=$(( $(date +%s) + $(_scale_timeout 30) )) listing=""
  until listing=$("${FLEET_BIN}" mic sources 2>&1) && printf '%s\n' "${listing}" | grep -Eq "^\* $1 "; do
    [ "$(date +%s)" -lt "${deadline}" ] || fail "'fleet mic sources' never marked $1 as the source: ${listing}"
    sleep 0.5
  done
}

info "nothing selected: desk attached last, so desk is the source"
wait_source desk
tui_wait_for "[ Automatic ]" 10 "${desk}"
tui_wait_for "[ Automatic ]" 10 "${laptop}"

# Walk the laptop TUI's cursor down to the Source row — by looking, not by
# counting: the rows above it vary with the tools installed.
info "on the laptop TUI, select this machine as the source"
found=""
for _ in $(seq 1 60); do
  if tui_capture "${laptop}" | grep -Eq '>[[:space:]]+Source'; then
    found=1
    break
  fi
  tui_send j "${laptop}"
  sleep 0.2
done
[ -n "${found}" ] || fail "could not reach the Source row: $(tui_capture "${laptop}")"
tui_send Right "${laptop}"
tui_wait_for "[ laptop (this machine)" 10 "${laptop}"
wait_source laptop

# THE POINT: the desk TUI was already open. It fetched the config before the
# selection was made, and must show the new one anyway.
info "the desk TUI, already open, shows the selection made on the laptop"
tui_wait_for "[ laptop · System default ]" 15 "${desk}"
! shows "${desk}" "[ Automatic ]" || fail "the desk TUI still shows the selection it started with: $(tui_capture "${desk}")"

# …and must not write its old copy back. The desk cursor is still on the first
# row, Tmux vim keys; toggling it saves the whole config from the desk TUI.
info "saving an unrelated setting from the desk TUI leaves the microphone source alone"
before=$(tui_capture "${desk}" | grep -F "Tmux vim keys" || true)
tui_send Enter "${desk}"
deadline=$(( $(date +%s) + $(_scale_timeout 10) ))
until [ "$(tui_capture "${desk}" | grep -F "Tmux vim keys" || true)" != "${before}" ]; do
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the desk TUI never saved the toggle: $(tui_capture "${desk}")"
  sleep 0.25
done
sleep 2 # long enough for a wrongly-reverted selection to reach the daemon and back
grep -Eq '"client": *"laptop"' "${HOME}/.fleet/config.json" \
  || fail "an unrelated save from the desk TUI rewrote the selection: $(cat "${HOME}/.fleet/config.json")"
wait_source laptop
shows "${laptop}" "[ laptop (this machine)" || fail "the laptop TUI lost its selection: $(tui_capture "${laptop}")"
shows "${desk}" "[ laptop · System default ]" || fail "the desk TUI lost the selection: $(tui_capture "${desk}")"

pass "microphone selection is shared by every open TUI"
