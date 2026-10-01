// Package mic is the CLIENT half of fleet's virtual microphone: it captures
// the microphone of the machine the human sits at (where the TUI runs) and
// hands raw PCM to the Mic gRPC stream, which the daemon fans out into fleet
// instances (see internal/server/mic.go and internal/micsink).
//
// fleet is built without cgo (the darwin binaries are cross-compiled from
// linux), so there is no audio binding here: capture shells out to whichever
// recorder the machine already has (PulseAudio/PipeWire's parec, ALSA's
// arecord, ffmpeg, SoX) and reads raw samples off its stdout — the same
// exec-and-pipe shape as the rest of fleet's data planes.
//
// The package is pure client code: it imports no daemon-side packages, so the
// TUI can use it under the depguard import boundary.
package mic

import (
	"os"
	"strings"
	"unicode"
)

// The wire format is fixed: signed 16-bit little-endian, 16 kHz, mono. That is
// exactly what speech-to-text front ends ask for (Claude Code's voice mode
// records `-f S16_LE -r 16000 -c 1`), so the common path is conversion-free;
// the in-instance PulseAudio source resamples for any consumer wanting more.
const (
	// SampleRate is the PCM sample rate in Hz.
	SampleRate = 16000
	// Channels is the PCM channel count.
	Channels = 1
	// BytesPerSecond is the PCM byte rate (16-bit mono).
	BytesPerSecond = SampleRate * Channels * 2
	// ChunkBytes bounds one captured frame: 40 ms of audio. Small enough that
	// the hop adds no audible latency, large enough to keep the frame rate (25/s)
	// trivial for the stream.
	ChunkBytes = BytesPerSecond / 25
)

// EnvCapture overrides the capture command entirely: its value is run with
// `sh -c` and its stdout must be raw PCM in the wire format above. It exists
// for exotic audio stacks fleet does not know how to drive, and lets tests feed
// a deterministic signal with no sound hardware (same role as FLEET_OPENER for
// `fleet open`). When set, device selection is ignored.
const EnvCapture = "FLEET_MIC_CAPTURE"

// EnvDevice is set in the recorder's environment to the device id the user
// selected ("" = system default). fleet's own recorders get the device as an
// argument and ignore it; it exists for an EnvCapture override, which fleet
// cannot pass a device to — so a custom recorder can still honour the selection
// (and a test can see which device a provider chose). It is the raw configured
// id, NOT validated against this machine's devices: an override that uses it
// must treat it as untrusted input, exactly as fleet does (see knownDevice).
const EnvDevice = "FLEET_MIC_DEVICE"

// EnvClient overrides the name this machine's provider announces to the daemon
// (default: the short hostname). The name is what Settings lists this machine's
// devices under and what the microphone selection is stored against, so two
// machines that share a hostname need it to be told apart.
const EnvClient = "FLEET_MIC_CLIENT"

// Bounds on what one provider may announce. A provider can be anywhere (a
// remote TUI), and what it announces is stored by the daemon and drawn in
// every OTHER client's terminal — so it is bounded, and scrubbed (CleanText),
// at both of those points.
const (
	MaxClientName  = 64
	MaxDevices     = 64
	MaxDeviceID    = 256
	MaxDeviceLabel = 128
)

// Device is one selectable capture device on this machine.
type Device struct {
	// ID is the stable identifier persisted in MicSettings.Device. It is
	// namespaced by the capture tool ("pulse:<source>", "alsa:<pcm>",
	// "avfoundation:<name>") because a native device name only means something
	// to the tool that enumerated it.
	ID string
	// Label is the human-readable name shown in the settings page.
	Label string
}

// DefaultLabel is how the settings page names the empty ("") device id.
const DefaultLabel = "System default"

// hostname is a seam for tests.
var hostname = os.Hostname

// ClientName is the name this machine's provider announces (MicOpen.client):
// EnvClient if set, else the short hostname. It must be STABLE — the microphone
// selection is stored against it — which is why it is a property of the machine
// and not of the process: two providers on one machine are the same source.
func ClientName() string {
	if name := CleanText(os.Getenv(EnvClient), MaxClientName); name != "" {
		return name
	}
	host, err := hostname()
	if err != nil {
		host = ""
	}
	// The short name: "desk.lan" and "desk.local" are the same machine on two
	// networks, and the selection has to survive the move.
	short, _, _ := strings.Cut(host, ".")
	if name := CleanText(short, MaxClientName); name != "" {
		return name
	}
	return "unknown"
}

// CleanText makes a string from another machine safe to store and to draw in a
// terminal: only printable runes survive (no control characters, so no escape
// sequences; no format characters, so no bidi overrides), whitespace runs
// collapse to one space, and the result is cut to limit runes.
func CleanText(text string, limit int) string {
	var b strings.Builder
	space, count := false, 0
	for _, r := range text {
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if !unicode.IsPrint(r) {
			continue
		}
		if space {
			if count+1 >= limit {
				break
			}
			b.WriteByte(' ')
			count++
			space = false
		}
		if count >= limit {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// CleanDevices applies the announcement bounds to a device listing: at most
// MaxDevices, each with a printable id (an id is an identifier — one that would
// have to be altered is dropped, never rewritten into a different one) and a
// scrubbed label (the id, if nothing printable is left).
func CleanDevices(devices []Device) []Device {
	var clean []Device
	seen := make(map[string]bool, len(devices))
	for _, device := range devices {
		if len(clean) == MaxDevices {
			break
		}
		if seen[device.ID] || !printableID(device.ID) {
			continue
		}
		seen[device.ID] = true
		label := CleanText(device.Label, MaxDeviceLabel)
		if label == "" {
			label = device.ID
		}
		clean = append(clean, Device{ID: device.ID, Label: label})
	}
	return clean
}

func printableID(id string) bool {
	if id == "" || len(id) > MaxDeviceID {
		return false
	}
	for _, r := range id {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
