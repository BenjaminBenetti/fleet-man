package startup

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/micsink"
)

// micConfigMarker tags the config files this script writes, so a re-run can tell
// fleet's own /etc/asound.conf (safe to rewrite) from one the image or user put
// there (left alone — and, if it leaves the ALSA default dead, reported).
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
fleet_bin='%[3]s'
# Test seam: a prefix for every system path this script reads or writes.
root="${FLEET_MIC_ROOT:-}"
asound="$root/etc/asound.conf"
client_conf="$root/etc/pulse/client.conf.d/00-fleet-mic.conf"

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

# The ALSA pulse plugin, wherever this distro keeps alsa-lib's plugins
# (debian: /usr/lib/<triplet>, fedora: /usr/lib64, alpine: /usr/lib). Globs
# rather than find: findutils is not a given on a slim image.
have_pulse_plugin() {
  for libdir in "$root"/usr/lib/alsa-lib "$root"/usr/lib64/alsa-lib "$root"/usr/lib/*/alsa-lib "$root"/lib/*/alsa-lib; do
    [ -e "$libdir/libasound_module_pcm_pulse.so" ] && return 0
  done
  return 1
}

installed() {
  command -v pulseaudio >/dev/null 2>&1 && command -v pactl >/dev/null 2>&1 &&
    command -v arecord >/dev/null 2>&1 && have_pulse_plugin
}

# Whose /etc/asound.conf is this? Decided BEFORE the install, because the
# install can create one: on RPM distros alsa-lib SHIPS a stock /etc/asound.conf,
# and judging it afterwards would make fleet refuse to touch a file its own
# install just dropped. Only a file that was already here, unmarked, is the
# image's or the user's — and only that one is left alone.
foreign_asound=""
if [ -e "$asound" ] && ! grep -qF "$marker" "$asound" 2>/dev/null; then
  foreign_asound=1
fi

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

# write_system <path>: install stdin at path.
write_system() {
  as_root mkdir -p "$(dirname "$1")" && as_root tee "$1" >/dev/null
}

if [ -n "$foreign_asound" ]; then
  echo "leaving the existing $asound alone (fleet did not write it)"
else
  write_system "$asound" <<CONF || exit 1
$marker
pcm.!default { type pulse }
ctl.!default { type pulse }
CONF
fi

# The drop-in is fleet's by name, so it is always (re)written.
write_system "$client_conf" <<CONF || exit 1
$marker
default-server = unix:$socket
autospawn = no
CONF

# Bring the virtual microphone up now, so a recorder probing for a device finds
# one before any client has attached. The staged fleet binary owns the server's
# configuration; an instance without it yet gets the server on first attach.
server_up=""
if [ -x "$fleet_bin" ]; then
  if "$fleet_bin" mic ensure; then
    server_up=1
  else
    echo "fleet mic ensure failed (the daemon retries on attach)"
  fi
fi

# alsa_default_reaches_pulse: does a plain ALSA recorder end up on PulseAudio?
# With the server up this is asked of ALSA itself — a recorder on a live default
# keeps running until killed (timeout: 124, busybox: 143), one on a dead default
# exits at once. Without it, fall back to reading the configs.
alsa_default_reaches_pulse() {
  if [ -n "$server_up" ] && command -v timeout >/dev/null 2>&1; then
    timeout 1 arecord -q -f S16_LE -r 16000 -c 1 -t raw /dev/null >/dev/null 2>&1
    rc=$?
    [ "$rc" = 124 ] || [ "$rc" = 143 ]
    return
  fi
  grep -qs 'type *pulse' "$asound" "$root"/etc/alsa/conf.d/*.conf "$root"/usr/share/alsa/alsa.conf.d/*.conf
}

# Declining to own the ALSA default is only fine if the default gets to
# PulseAudio anyway (the image routes it itself, or the pulse plugin's drop-in
# does). Otherwise say so LOUDLY: everything else here works, the provider goes
# "live" — and arecord (so: Claude Code's voice mode) records pure silence.
if [ -n "$foreign_asound" ] && ! alsa_default_reaches_pulse; then
  echo "WARNING: $asound is not fleet's and does not route the ALSA default to PulseAudio."
  echo "ALSA recorders (arecord, Claude Code voice mode) will NOT hear fleet's microphone."
  echo "Add 'pcm.!default { type pulse }' to it, or remove it and rebuild the instance."
  exit 3
fi
echo "virtual microphone ready"`, micConfigMarker, micsink.SocketPath, fleetlaunch.RemotePath),
	}
}
