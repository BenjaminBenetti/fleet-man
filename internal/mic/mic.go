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
