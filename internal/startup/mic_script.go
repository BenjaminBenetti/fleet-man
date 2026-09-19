package startup

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/micsink"
)

// micConfigMarker tags the config files this script writes, so a re-run can tell
// fleet's own file (safe to rewrite) from one the image or user put there (left
// alone).
const micConfigMarker = "# managed by fleet (virtual microphone)"

// MicScript installs what an instance needs to expose fleet's virtual
// microphone (see internal/micsink for why it is PulseAudio):
//
//   - pulseaudio + pactl: the private sound server and its control tool;
//   - the ALSA pulse plugin + /etc/asound.conf: makes the ALSA "default" device
//     the virtual microphone, which is what arecord, SoX, ffmpeg and native ALSA
//     clients (Claude Code's voice mode among them) open;
//   - alsa-utils: arecord itself — the recorder voice front ends fall back to;
//   - a pulse client.conf.d drop-in pointing every pulse client at the private
//     server's socket, so nothing depends on PULSE_SERVER reaching a process's
//     environment.
//
// Unlike the agent install scripts this one is not tied to a FleetSettings
// toggle: the microphone is a global setting, so callers (provisioning, and the
// daemon's lazy install for instances created before the feature was turned on)
// add it explicitly. It is idempotent and quick when everything is in place.
// Needs root or passwordless sudo, like every package install fleet does.
func MicScript() Script {
	return Script{
		Name: "mic",
		Body: fmt.Sprintf(`marker='%[1]s'
socket='%[2]s'

as_root() {
  if [ "$(id -u)" = 0 ]; then
    "$@"
  elif command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then
    sudo -n "$@"
  else
    echo "need root or passwordless sudo to run: $*"
    return 126
  fi
}

have_pulse_plugin() {
  find /usr/lib /usr/lib64 /lib -name 'libasound_module_pcm_pulse.so' 2>/dev/null | grep -q .
}

installed() {
  command -v pulseaudio >/dev/null 2>&1 && command -v pactl >/dev/null 2>&1 &&
    command -v arecord >/dev/null 2>&1 && have_pulse_plugin
}

if installed; then
  echo "audio packages already installed"
elif command -v apt-get >/dev/null 2>&1; then
  # update and install are separate statements: a stale third-party repo makes
  # update exit non-zero on images where the install would still succeed.
  as_root env DEBIAN_FRONTEND=noninteractive apt-get update -qq
  as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
    pulseaudio pulseaudio-utils libasound2-plugins alsa-utils
elif command -v apk >/dev/null 2>&1; then
  as_root apk add --no-cache pulseaudio pulseaudio-utils alsa-plugins-pulse alsa-utils
elif command -v dnf >/dev/null 2>&1; then
  as_root dnf install -y --allowerasing pulseaudio pulseaudio-utils alsa-plugins-pulseaudio alsa-utils
else
  echo "no supported package manager (apt-get, apk, dnf) found"
  exit 1
fi
if ! installed; then
  echo "audio packages are still missing after install"
  exit 1
fi

# write_managed <path>: install stdin at path unless a file fleet did not write
# is already there.
write_managed() {
  if [ -e "$1" ] && ! grep -qF "$marker" "$1" 2>/dev/null; then
    echo "leaving existing $1 alone"
    cat >/dev/null
    return 0
  fi
  as_root mkdir -p "$(dirname "$1")" && as_root tee "$1" >/dev/null
}

write_managed /etc/asound.conf <<CONF || exit 1
$marker
pcm.!default { type pulse }
ctl.!default { type pulse }
CONF

write_managed /etc/pulse/client.conf.d/00-fleet-mic.conf <<CONF || exit 1
$marker
default-server = unix:$socket
autospawn = no
CONF

# Bring the virtual microphone up now, so a recorder probing for a device finds
# one before any client has attached. The staged fleet binary owns the server's
# configuration; an instance without it yet gets the server on first attach.
if command -v fleet >/dev/null 2>&1; then
  fleet mic ensure || echo "fleet mic ensure failed (the daemon retries on attach)"
fi
echo "virtual microphone ready"`, micConfigMarker, micsink.SocketPath),
	}
}
