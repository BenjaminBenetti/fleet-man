package mic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// probeTimeout bounds every helper invocation made while detecting a capture
// tool or listing devices. These run from the settings page, so a wedged sound
// server must not freeze the UI for long.
const probeTimeout = 3 * time.Second

// Seams for tests: the host OS, PATH lookup, and "run this and give me its
// combined output".
var (
	goos     = runtime.GOOS
	lookPath = exec.LookPath
	runProbe = func(name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		// The output is parsed, and pactl's "Name:" / "Description:" labels are
		// gettext-translated: pin the locale or a non-English desktop lists
		// no devices at all.
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		cmd.WaitDelay = time.Second
		return cmd.CombinedOutput()
	}
)

// tool is one way of recording on this machine.
type tool struct {
	// name namespaces the tool's device ids (Device.ID = name + ":" + native).
	name string
	// bin is the executable that must be on PATH.
	bin string
	// usable is an optional extra check beyond bin being on PATH (e.g. parec is
	// useless without a reachable sound server). It is handed the one sound-
	// server probe detection already made, rather than each making its own.
	usable func(pulse pulseProbe) bool
	// args builds the argv (after bin) recording the wire format to stdout.
	// native is the tool's own device name; "" means the system default.
	args func(native string) []string
	// list enumerates the tool's capture devices. nil means the tool can only
	// record the system default.
	list func() ([]Device, error)
}

var rateArg = strconv.Itoa(SampleRate)
var channelsArg = strconv.Itoa(Channels)

// pulseTool records through PulseAudio — or PipeWire's pulse shim, which is what
// nearly every current Linux desktop (and WSLg) actually runs.
var pulseTool = tool{
	name: "pulse",
	bin:  "parec",
	usable: func(pulse pulseProbe) bool {
		// No pactl to ask: let parec try — it fails fast with no server.
		return !pulse.havePactl || pulse.answers
	},
	args: func(native string) []string {
		args := []string{"--raw", "--format=s16le", "--rate=" + rateArg, "--channels=" + channelsArg,
			"--latency-msec=20", "--client-name=fleet"}
		if native != "" {
			args = append(args, "--device="+native)
		}
		return args
	},
	list: func() ([]Device, error) {
		out, err := runProbe("pactl", "list", "sources")
		if err != nil {
			return nil, fmt.Errorf("pactl list sources: %w", err)
		}
		return parsePactlSources(string(out)), nil
	},
}

// alsaTool records straight from ALSA.
var alsaTool = tool{
	name: "alsa",
	bin:  "arecord",
	args: func(native string) []string {
		args := []string{"-q", "-t", "raw", "-f", "S16_LE", "-r", rateArg, "-c", channelsArg}
		if native != "" {
			args = append(args, "-D", native)
		}
		return append(args, "-")
	},
	list: func() ([]Device, error) {
		out, err := runProbe("arecord", "-L")
		if err != nil {
			return nil, fmt.Errorf("arecord -L: %w", err)
		}
		return parseArecordPCMs(string(out)), nil
	},
}

// avfoundationTool records on macOS through ffmpeg's AVFoundation input.
var avfoundationTool = tool{
	name: "avfoundation",
	bin:  "ffmpeg",
	args: func(native string) []string {
		if native == "" {
			native = "default"
		}
		return ffmpegArgs("avfoundation", ":"+native)
	},
	list: func() ([]Device, error) {
		// ffmpeg "fails" this invocation by design (there is no input to open),
		// so its exit status says nothing; the device table is on stderr
		// regardless. What DOES mean failure is the table not being there.
		out, _ := runProbe("ffmpeg", "-hide_banner", "-f", "avfoundation", "-list_devices", "true", "-i", "")
		if !strings.Contains(string(out), avfoundationAudioHeading) {
			return nil, fmt.Errorf("ffmpeg printed no AVFoundation device table")
		}
		return parseAVFoundationAudio(string(out)), nil
	},
}

// ffmpegALSATool is the Linux fallback for a machine that has ffmpeg but
// neither pulseaudio-utils nor alsa-utils. Default device only.
var ffmpegALSATool = tool{
	name: "ffmpeg",
	bin:  "ffmpeg",
	args: func(string) []string { return ffmpegArgs("alsa", "default") },
}

// soxTool is the last resort on both platforms. Default device only.
var soxTool = tool{
	name: "sox",
	bin:  "rec",
	args: func(string) []string {
		// -L: the wire format is little-endian by contract, not host-native.
		return []string{"-q", "-t", "raw", "-r", rateArg, "-e", "signed", "-b", "16", "-c", channelsArg, "-L", "-"}
	},
}

func ffmpegArgs(format, input string) []string {
	return []string{"-nostdin", "-hide_banner", "-loglevel", "error",
		"-fflags", "nobuffer", "-f", format, "-i", input,
		"-ac", channelsArg, "-ar", rateArg, "-f", "s16le", "-flush_packets", "1", "-"}
}

// candidates lists the capture tools worth trying on this OS, best first.
func candidates() []tool {
	if goos == "darwin" {
		return []tool{avfoundationTool, soxTool}
	}
	return []tool{pulseTool, alsaTool, ffmpegALSATool, soxTool}
}

// detectTTL is how long a detection verdict is reused. Detection costs a
// `pactl info` round trip (up to probeTimeout against a wedged sound server),
// and it sits on the capture-start path where every millisecond is clipped off
// the start of the user's sentence.
const detectTTL = time.Minute

var detectCache struct {
	mu    sync.Mutex
	at    time.Time
	found tool
	err   error
	// epoch counts detection verdicts. A device listing started under one
	// verdict must not be stored under the next (the tool may have changed).
	epoch int
	// devices is the allowlist for knownDevice: the FULL device ids
	// ("pulse:<name>") the detected tool listed — full ids, so a name one tool
	// enumerated can never validate under another tool's prefix. nil = not
	// listed yet. devicesAt is when it was listed.
	devices   map[string]bool
	devicesAt time.Time
}

// deviceListTTL bounds how often a device id that is NOT in the allowlist can
// trigger a re-listing. Without it a stale id — exactly the case the fallback
// exists for — would cost a full enumeration (up to probeTimeout against a
// wedged sound server) on every capture start. A device plugged in since the
// last listing becomes usable within this long, or at once when the settings
// page lists devices.
const deviceListTTL = 10 * time.Second

// detect returns the capture tool to use, caching the verdict briefly. The
// error says WHY there is none: ErrNoCaptureTool or ErrVirtualMic.
func detect() (tool, error) {
	detectCache.mu.Lock()
	defer detectCache.mu.Unlock()
	if !detectCache.at.IsZero() && time.Since(detectCache.at) < detectTTL {
		return detectCache.found, detectCache.err
	}
	detectCache.found, detectCache.err = detectUncached()
	detectCache.at = time.Now()
	detectCache.epoch++
	detectCache.devices = nil
	return detectCache.found, detectCache.err
}

// resetDetectCache forgets the cached verdict (tests, and the settings page's
// explicit device refresh).
func resetDetectCache() {
	detectCache.mu.Lock()
	detectCache.at = time.Time{}
	detectCache.epoch++
	detectCache.devices = nil
	detectCache.mu.Unlock()
}

// VirtualSourceName is the name of the virtual microphone fleet creates inside
// an instance (internal/micsink). Shared here so the capture side can recognise
// it without importing the in-instance package.
const VirtualSourceName = "fleetmic"

// pulseProbe is what one `pactl info` told detection.
type pulseProbe struct {
	havePactl bool
	answers   bool
	// virtualMic: the default source is fleet's OWN virtual microphone — this
	// process runs inside a fleet instance.
	virtualMic bool
}

func probePulse() pulseProbe {
	if _, err := lookPath("pactl"); err != nil {
		return pulseProbe{}
	}
	probe := pulseProbe{havePactl: true}
	out, err := runProbe("pactl", "info")
	if err != nil {
		return probe
	}
	probe.answers = true
	for line := range strings.SplitSeq(string(out), "\n") {
		// Whole-line match: "fleetmic" must not match "fleetmicrophone_usb".
		if strings.TrimSpace(line) == "Default Source: "+VirtualSourceName {
			probe.virtualMic = true
		}
	}
	return probe
}

func detectUncached() (tool, error) {
	pulse := probePulse()
	if pulse.virtualMic {
		// Recording fleet's own virtual microphone would be a loop: the
		// recorder is itself "something recording in the instance", so it would
		// raise the very demand that keeps it running, and feed the instance
		// its own silence. Such a machine has no microphone to offer.
		return tool{}, ErrVirtualMic
	}
	for _, candidate := range candidates() {
		if _, err := lookPath(candidate.bin); err != nil {
			continue
		}
		if candidate.usable != nil && !candidate.usable(pulse) {
			continue
		}
		return candidate, nil
	}
	return tool{}, ErrNoCaptureTool
}

// knownDevice reports whether deviceID is a device the detected tool actually
// lists on THIS machine. MicSettings.Device is daemon-side config: it can come
// from a client on another machine — or from a daemon the user only half
// trusts — and it is about to become an argument to a recorder. ALSA device
// strings in particular are a small language ("plugin:args") in which some
// plugins take a FILENAME, so an arbitrary string from the wire must never
// reach `arecord -D`. Only ids this machine enumerated itself get through;
// anything else records the system default. This is the single enforcement
// point for that, so it is built to hold unconditionally: the allowlist holds
// FULL ids (tool prefix included), and a listing is only stored if the
// detection verdict it was made under is still the current one.
func knownDevice(detected tool, deviceID string) bool {
	if detected.list == nil {
		return false
	}
	detectCache.mu.Lock()
	known, listedAt, epoch := detectCache.devices, detectCache.devicesAt, detectCache.epoch
	detectCache.mu.Unlock()
	if known != nil && (known[deviceID] || time.Since(listedAt) < deviceListTTL) {
		return known[deviceID]
	}

	// Not listed yet, or a miss on a listing old enough that the device may
	// have been plugged in since: list again.
	devices, err := detected.list()
	if err != nil {
		// A probe that FAILED says nothing about the hardware. It must not
		// replace a good allowlist with an empty one — that would switch the
		// user to a different microphone for a whole TTL because the sound
		// server was busy for a moment. Keep what we knew, and only push the
		// timestamp forward so a wedged server is not re-probed on every capture
		// start. (With no earlier listing there is nothing to keep, and an
		// unvalidated id still must not reach the recorder.)
		detectCache.mu.Lock()
		if detectCache.epoch == epoch && detectCache.devices != nil {
			detectCache.devicesAt = time.Now()
		}
		detectCache.mu.Unlock()
		return known[deviceID]
	}
	return storeDevices(epoch, devices)[deviceID]
}

// storeDevices records a listing as the allowlist, unless detection has moved
// on since epoch, and returns the set either way.
func storeDevices(epoch int, devices []Device) map[string]bool {
	known := make(map[string]bool, len(devices))
	for _, device := range devices {
		known[device.ID] = true
	}
	detectCache.mu.Lock()
	if detectCache.epoch == epoch {
		detectCache.devices, detectCache.devicesAt = known, time.Now()
	}
	detectCache.mu.Unlock()
	return known
}

// ErrNoCaptureTool is returned when this machine has nothing fleet can record
// with. InstallHint says what to install.
var ErrNoCaptureTool = errors.New("no audio capture tool found")

// ErrVirtualMic is returned when this machine's only microphone is the virtual
// one fleet itself provides — i.e. fleet is running inside a fleet instance.
var ErrVirtualMic = errors.New("this machine's microphone is fleet's own virtual microphone (running inside a fleet instance)")

// InstallHint names the package that provides a capture tool on this OS.
func InstallHint() string {
	if goos == "darwin" {
		return "install ffmpeg (brew install ffmpeg)"
	}
	return "install pulseaudio-utils (parec) or alsa-utils (arecord)"
}

// Available reports whether this machine can capture at all: either the
// override command is set or a known recorder is present.
func Available() bool { return Unavailable() == nil }

// Unavailable says why this machine cannot capture (ErrNoCaptureTool,
// ErrVirtualMic), or nil if it can. Describe turns it into user-facing text.
func Unavailable() error {
	if os.Getenv(EnvCapture) != "" {
		return nil
	}
	_, err := detect()
	return err
}

// Describe renders a capture-availability error for the user: what is wrong
// and, where there is something to do about it, what.
func Describe(err error) string {
	if errors.Is(err, ErrNoCaptureTool) {
		return "no capture tool: " + InstallHint()
	}
	return err.Error()
}

// Devices enumerates the capture devices the settings page offers, NOT including
// the implicit system default (the empty id). An override command or a
// default-only tool yields an empty list.
func Devices() ([]Device, error) {
	if os.Getenv(EnvCapture) != "" {
		return nil, nil
	}
	// Listing is an explicit user action (opening the selector), so re-detect:
	// they may have just installed a recorder.
	resetDetectCache()
	detected, err := detect()
	if err != nil {
		return nil, err
	}
	if detected.list == nil {
		return nil, nil
	}
	detectCache.mu.Lock()
	epoch := detectCache.epoch
	detectCache.mu.Unlock()
	devices, err := detected.list()
	if err != nil {
		return nil, err
	}
	// What the user is about to pick from is, by construction, the allowlist.
	storeDevices(epoch, devices)
	return devices, nil
}

// recorderArgv is the recorder to run for deviceID ("" = system default), as an
// argv. A deviceID this machine did not itself enumerate — another tool's,
// another machine's, unplugged, or not a device name at all (see knownDevice) —
// records the default instead. The second result is the device id actually
// used, so callers can tell the user when that differs from what they chose.
//
// It is only ever RUN through guardedCommand, which ties the recorder's
// lifetime to fleet's.
func recorderArgv(deviceID string) (argv []string, used string, err error) {
	if override := os.Getenv(EnvCapture); override != "" {
		return []string{"sh", "-c", override}, "", nil
	}
	detected, err := detect()
	if err != nil {
		return nil, "", err
	}
	native := ""
	if toolName, rest, found := strings.Cut(deviceID, ":"); found && toolName == detected.name && knownDevice(detected, deviceID) {
		native = rest
	}
	if native != "" {
		used = deviceID
	}
	return append([]string{detected.bin}, detected.args(native)...), used, nil
}

// --- enumeration parsers ------------------------------------------------------

// parsePactlSources reads `pactl list sources` (the long form — the short form
// has no descriptions). Monitor sources are skipped: they capture what the
// machine is PLAYING, which is never what a microphone setting means.
func parsePactlSources(out string) []Device {
	var devices []Device
	var name string
	flush := func(description string) {
		if name == "" || strings.HasSuffix(name, ".monitor") {
			name = ""
			return
		}
		if description == "" {
			description = name
		}
		devices = append(devices, Device{ID: "pulse:" + name, Label: description})
		name = ""
	}
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Name: "):
			flush("") // a source with no Description line
			name = strings.TrimPrefix(line, "Name: ")
		case strings.HasPrefix(line, "Description: "):
			flush(strings.TrimPrefix(line, "Description: "))
		}
	}
	flush("")
	return devices
}

// parseArecordPCMs reads `arecord -L`: each PCM is an unindented name followed
// by indented description lines. Only the per-card "plughw:" entries are kept —
// they are the real capture hardware, with format conversion, which is what a
// fixed 16 kHz mono request needs; the rest (null, default, surround, dsnoop…)
// are plumbing.
func parseArecordPCMs(out string) []Device {
	var devices []Device
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if line == "" || line[0] == ' ' || line[0] == '\t' || !strings.HasPrefix(line, "plughw:") {
			continue
		}
		label := line
		if i+1 < len(lines) {
			if description := strings.TrimSpace(lines[i+1]); description != "" && (lines[i+1][0] == ' ' || lines[i+1][0] == '\t') {
				label = description
			}
		}
		devices = append(devices, Device{ID: "alsa:" + line, Label: label})
	}
	return devices
}

const avfoundationAudioHeading = "AVFoundation audio devices"

// parseAVFoundationAudio reads ffmpeg's AVFoundation device table, keeping the
// entries under the "audio devices" heading:
//
//	[AVFoundation indev @ 0x…] AVFoundation audio devices:
//	[AVFoundation indev @ 0x…] [0] MacBook Pro Microphone
//
// The device NAME is the id (ffmpeg accepts it in place of the index), because
// indexes shift whenever a device is plugged in.
func parseAVFoundationAudio(out string) []Device {
	var devices []Device
	inAudio := false
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, avfoundationAudioHeading) {
			inAudio = true
			continue
		}
		if strings.Contains(line, "AVFoundation video devices") {
			inAudio = false
			continue
		}
		if !inAudio {
			continue
		}
		// Strip the "[AVFoundation indev @ …] " log prefix, then "[N] ".
		_, rest, found := strings.Cut(line, "] ")
		if !found || !strings.HasPrefix(rest, "[") {
			continue
		}
		_, name, found := strings.Cut(rest, "] ")
		name = strings.TrimSpace(name)
		if !found || name == "" {
			continue
		}
		devices = append(devices, Device{ID: "avfoundation:" + name, Label: name})
	}
	return devices
}
