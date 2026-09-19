package startup

import (
	"fmt"

	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/micsink"
)

// micConfigMarker tags what this script writes: the pulse client drop-in, and
// the begin/end lines of fleet's block inside /etc/asound.conf — so a re-run can
// replace exactly its own lines and nothing else in that file.
const micConfigMarker = "managed by fleet (virtual microphone)"

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
# Test seam: a prefix for every system path this script reads, writes or runs.
root="${FLEET_MIC_ROOT:-}"
fleet_bin="$root%[3]s"
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

# /etc/asound.conf is shared ground, so ownership is decided by CONTENT, not by
# whether the file exists (RPM distros ship a stock one with alsa-lib — possibly
# installed by this very script, possibly long before it ever ran):
#   - fleet owns only its own delimited block, which a re-run replaces;
#   - everything else in the file is kept, verbatim;
#   - the one thing fleet will not do is override a default the image or user
#     set: a file that itself claims pcm.!default / ctl.!default is left alone
#     (and checked below for whether that default reaches PulseAudio anyway).
begin="# >>> $marker >>>"
end="# <<< $marker <<<"
others=""
if [ -e "$asound" ]; then
  if head -n 1 "$asound" | grep -qxF "# $marker"; then
    : # an early fleet build wrote the whole file under a bare marker line
  else
    others=$(sed "/^$begin\$/,/^$end\$/d" "$asound")
  fi
fi
foreign_asound=""
if printf '%%s\n' "$others" | grep -Eq '^[[:space:]]*(pcm|ctl)\.!default'; then
  foreign_asound=1
  echo "leaving the existing $asound alone (it sets its own ALSA default)"
else
  {
    [ -z "$others" ] || printf '%%s\n' "$others"
    printf '%%s\n' "$begin" 'pcm.!default { type pulse }' 'ctl.!default { type pulse }' "$end"
  } | write_system "$asound" || exit 1
fi

# The drop-in is fleet's by name, so it is always (re)written.
write_system "$client_conf" <<CONF || exit 1
# $marker
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

# default_is_pulse <file>...: does one of these ALSA configs define pcm.!default
# as a pulse device? It has to be the !default — "type pulse" merely appearing
# somewhere (a pcm.pulse definition, which the plugin's own drop-in always
# carries) says nothing about where the default goes.
default_is_pulse() {
  for conf in "$@"; do
    [ -r "$conf" ] || continue
    awk '
      /pcm\.!default/ { in_default = 1 }
      in_default && /type[ \t]+"?pulse"?/ { found = 1 }
      in_default && /}/ { in_default = 0 }
      END { exit !found }
    ' "$conf" && return 0
  done
  return 1
}

# alsa_default_reaches_pulse: does a plain ALSA recorder end up on fleet's
# PulseAudio server? "It keeps recording" is not the question — a default of
# type null (a common way for an image to silence ALSA) or type hw records
# happily, and records nothing of ours. With the server up, ask the server: start
# a recorder on the ALSA default and require the private server to see its
# stream. Without it, read the configs: the foreign file wins if it defines
# !default at all; otherwise the drop-ins decide.
alsa_default_reaches_pulse() {
  if [ -n "$server_up" ]; then
    arecord -q -f S16_LE -r 16000 -c 1 -t raw /dev/null >/dev/null 2>&1 &
    probe=$!
    reached=1
    for _ in 1 2 3; do
      # A recorder on the fleet microphone specifically (join on the source
      # index), not on the null sink's monitor.
      mic_index=$(PULSE_SERVER="unix:$socket" pactl list short sources 2>/dev/null | awk -v name='%[4]s' '$2 == name { print $1 }')
      if [ -n "$mic_index" ] && PULSE_SERVER="unix:$socket" pactl list short source-outputs 2>/dev/null |
        awk -v idx="$mic_index" '$2 == idx { found = 1 } END { exit !found }'; then
        reached=0
        break
      fi
      kill -0 "$probe" 2>/dev/null || break
      sleep 1
    done
    kill "$probe" 2>/dev/null
    wait "$probe" 2>/dev/null
    return "$reached"
  fi
  if grep -qs 'pcm\.!default' "$asound"; then
    default_is_pulse "$asound"
    return
  fi
  default_is_pulse "$root"/etc/alsa/conf.d/*.conf "$root"/usr/share/alsa/alsa.conf.d/*.conf
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
echo "virtual microphone ready"`, micConfigMarker, micsink.SocketPath, fleetlaunch.RemotePath, micsink.SourceName),
	}
}
