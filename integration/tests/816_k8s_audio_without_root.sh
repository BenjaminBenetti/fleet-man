#!/usr/bin/env bash
# itest: k8s
# Description: on a cluster that runs containers without root (a namespace enforcing the restricted pod security standard), an image fleet builds gets the audio stack in a layer of its own and plays audio output, while a prebuilt image without PulseAudio and ALSA says so: the audio-unavailable banner on the instance, in its creation log, and under Settings -> Audio Output.
set -euo pipefail

source "$(dirname "$0")/../common.sh"
NS=""
workdir=$(mktemp -d)
attach_pid=""
itest_cleanup() {
  tui_kill
  [ -z "${attach_pid}" ] || kill "${attach_pid}" 2>/dev/null || true
  if [ -n "${NS}" ]; then
    _be_kubectl delete namespace "${NS}" --wait=false >/dev/null 2>&1 || true
  fi
  rm -rf "${workdir}"
}
itest_begin

itest_backend_is kubernetes || pass "kubernetes only: audio on a cluster without root"

setup_test

# A namespace of its own, enforcing restricted, as a second connection.
NS="${FLEET_ITEST_NAMESPACE}-noroot"
_be_kubectl create namespace "${NS}" >/dev/null 2>&1 || true
_be_kubectl label --overwrite namespace "${NS}" pod-security.kubernetes.io/enforce=restricted >/dev/null
out=$("${FLEET_BIN}" cluster add "${FLEET_ITEST_CLUSTER}" --name noroot -n "${NS}" 2>&1) || fail "cluster add: ${out}"

info "audio output on, this client's speaker selected"
printf '{ "output_settings": { "enabled": true, "client": "itest-desk", "device": "pulse:itest_speaker" } }\n' > "${HOME}/.fleet/config.json.new"
mv "${HOME}/.fleet/config.json.new" "${HOME}/.fleet/config.json"
itest_config_written

# --- a prebuilt image the cluster pulls as it is: no audio stack ---

cat >"${FIXTURE_REPO_DIR}/.devcontainer/devcontainer.json" <<'JSON'
{
  "name": "fleet-man prebuilt fixture",
  "image": "mcr.microsoft.com/devcontainers/base:debian-12",
  "remoteUser": "vscode"
}
JSON
(cd "${FIXTURE_REPO_DIR}" && git commit -qam "fixture: prebuilt image")

info "fleet up prebuilt on the restricted connection"
"${FLEET_BIN}" up prebuilt --repo "${FIXTURE_REPO_URL}" --cluster noroot
prebuilt="${FIXTURE_REPO_NAME}/prebuilt"
uid=$("${FLEET_BIN}" exec "${prebuilt}" -- id -u)
[ "${uid}" != "0" ] || fail "the restricted pod runs as root"

want="audio unavailable on cluster noroot: containers can't run as root. Use an image with PulseAudio and ALSA installed."
warn_file="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-prebuilt.warn"
assert_contains "$(cat "${warn_file}" 2>/dev/null)" "${want}" "the instance's banner"
creation_log="${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}/prebuilt/creation.log"
if [ -f "${creation_log}" ]; then
  assert_contains "$(cat "${creation_log}")" "${want}" "the create's log"
fi
apt_log=$("${FLEET_BIN}" exec "${prebuilt}" -- sh -c 'cat /var/log/apt/history.log 2>/dev/null || true')
assert_not_contains "${apt_log}" "pulseaudio" "an install with no root"

# --- an image fleet builds: the audio layer, and output that plays ---

mkdir -p "${FIXTURE_REPO_DIR}/.devcontainer"
cat >"${FIXTURE_REPO_DIR}/.devcontainer/Dockerfile" <<'DOCKERFILE'
FROM mcr.microsoft.com/devcontainers/base:debian-12
RUN echo built-by-fleet > /etc/fleet-itest-built
DOCKERFILE
cat >"${FIXTURE_REPO_DIR}/.devcontainer/devcontainer.json" <<'JSON'
{
  "name": "fleet-man built fixture",
  "build": { "dockerfile": "Dockerfile" },
  "remoteUser": "vscode"
}
JSON
(cd "${FIXTURE_REPO_DIR}" && git add -A && git commit -qm "fixture: built image")

info "fleet up built on the restricted connection"
"${FLEET_BIN}" up built --repo "${FIXTURE_REPO_URL}" --cluster noroot
built="${FIXTURE_REPO_NAME}/built"
assert_equals "built-by-fleet" "$("${FLEET_BIN}" exec "${built}" -- cat /etc/fleet-itest-built)" "the instance runs the built image"
"${FLEET_BIN}" exec "${built}" -- sh -c 'command -v pulseaudio && command -v pactl && command -v parec && command -v arecord' >/dev/null \
  || fail "the built image has no audio stack: the layer was not added"
assert_not_contains "$(cat "${HOME}/.fleet/logs/${FIXTURE_REPO_NAME}-built.warn" 2>/dev/null || true)" "audio unavailable" "the built instance's banner"

# The client's PulseAudio tools, as 745 stands them in.
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

info "attach as the audio output client, and play noise in the built instance"
attach_log="${workdir}/attach.log"
PATH="${workdir}/bin:${PATH}" FLEET_MIC_CLIENT=itest-desk "${FLEET_BIN}" output attach > "${attach_log}" 2>&1 &
attach_pid=$!
deadline=$(( $(date +%s) + $(_scale_timeout 180) ))
until [ "$(nonzero_played)" -ge 192000 ]; do
  kill -0 "${attach_pid}" 2>/dev/null || fail "fleet output attach exited: $(cat "${attach_log}")"
  [ "$(date +%s)" -lt "${deadline}" ] || fail "the client played only $(nonzero_played) non-zero bytes: $(cat "${attach_log}"; grep -i 'audio output' "${HOME}/.fleet/fleet.log" | tail -5)"
  "${FLEET_BIN}" exec "${built}" -- sh -c 'head -c 384000 /dev/urandom | pacat --raw --format=s16le --rate=48000 --channels=2' \
    >/dev/null 2>&1 || sleep 1
done
info "client played $(nonzero_played) non-zero bytes from the built instance"
if grep -q "audio output source attached.*${prebuilt}" "${HOME}/.fleet/fleet.log"; then
  fail "an output source was opened in the instance without audio"
fi

info "Settings -> Audio Output counts the instance without audio"
tui_spawn
tui_wait_for "built" 30 || fail "the TUI never listed the instances"
tui_cursor_to "settings"
tui_send Enter
tui_wait_for "Tmux vim keys" 10
# The next section's row, so the Audio Output rows above it are in view.
tui_cursor_to "Fleet status mod"
tui_wait_for "no audio on 1 instance (cluster noroot: no root)" 20 \
  || fail "Settings does not count the instance without audio: $(tui_capture)"

pass "audio on a cluster without root: a built image plays, a prebuilt one says why it cannot"
