package mic

import (
	"context"
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
	// useless without a reachable sound server).
	usable func() bool
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
	usable: func() bool {
		if _, err := lookPath("pactl"); err != nil {
			// No pactl to ask; let parec try — it fails fast with no server.
			return true
		}
		_, err := runProbe("pactl", "info")
		return err == nil
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
		// ffmpeg "fails" this invocation by design (there is no input to open);
		// the device table is on stderr regardless.
		out, _ := runProbe("ffmpeg", "-hide_banner", "-f", "avfoundation", "-list_devices", "true", "-i", "")
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
		return []string{"-q", "-t", "raw", "-r", rateArg, "-e", "signed", "-b", "16", "-c", channelsArg, "-"}
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

// detectTTL is how long a detection verdict is reused. Detection can cost a
// `pactl info` round trip, and it sits on the capture-start path where every
// millisecond is clipped off the start of the user's sentence.
const detectTTL = time.Minute

var detectCache struct {
	mu    sync.Mutex
	at    time.Time
	found tool
	ok    bool
}

// detect returns the first usable capture tool, caching the verdict briefly.
func detect() (tool, bool) {
	detectCache.mu.Lock()
	defer detectCache.mu.Unlock()
	if !detectCache.at.IsZero() && time.Since(detectCache.at) < detectTTL {
		return detectCache.found, detectCache.ok
	}
	detectCache.found, detectCache.ok = detectUncached()
	detectCache.at = time.Now()
	return detectCache.found, detectCache.ok
}

// resetDetectCache forgets the cached verdict (tests, and the settings page's
// explicit device refresh).
func resetDetectCache() {
	detectCache.mu.Lock()
	detectCache.at = time.Time{}
	detectCache.mu.Unlock()
}

// VirtualSourceName is the name of the virtual microphone fleet creates inside
// an instance (internal/micsink). Shared here so the capture side can recognise
// it without importing the in-instance package.
const VirtualSourceName = "fleetmic"

// defaultIsVirtualMic reports whether this machine's default source is fleet's
// OWN virtual microphone — i.e. this process runs inside a fleet instance.
// Recording it would be a loop: the recorder is itself "something recording in
// the instance", so it would raise the very demand that keeps it running, and
// feed the instance its own silence. Such a machine has no microphone to offer.
func defaultIsVirtualMic() bool {
	if _, err := lookPath("pactl"); err != nil {
		return false
	}
	out, err := runProbe("pactl", "info")
	return err == nil && strings.Contains(string(out), "Default Source: "+VirtualSourceName)
}

func detectUncached() (tool, bool) {
	if defaultIsVirtualMic() {
		return tool{}, false
	}
	for _, candidate := range candidates() {
		if _, err := lookPath(candidate.bin); err != nil {
			continue
		}
		if candidate.usable != nil && !candidate.usable() {
			continue
		}
		return candidate, true
	}
	return tool{}, false
}

// ErrNoCaptureTool is returned when this machine has nothing fleet can record
// with. InstallHint says what to install.
var ErrNoCaptureTool = fmt.Errorf("no audio capture tool found")

// InstallHint names the package that provides a capture tool on this OS.
func InstallHint() string {
	if goos == "darwin" {
		return "install ffmpeg (brew install ffmpeg)"
	}
	return "install pulseaudio-utils (parec) or alsa-utils (arecord)"
}

// Available reports whether this machine can capture at all: either the
// override command is set or a known recorder is present.
func Available() bool {
	if os.Getenv(EnvCapture) != "" {
		return true
	}
	_, ok := detect()
	return ok
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
	detected, ok := detect()
	if !ok {
		return nil, ErrNoCaptureTool
	}
	if detected.list == nil {
		return nil, nil
	}
	return detected.list()
}

// Command builds the unstarted capture command for deviceID ("" = system
// default). A deviceID that belongs to a different tool than the one this
// machine has — a config shared with a client on another machine — records the
// default instead of failing. The second result is the device id actually used.
func Command(ctx context.Context, deviceID string) (*exec.Cmd, string, error) {
	if override := os.Getenv(EnvCapture); override != "" {
		return exec.CommandContext(ctx, "sh", "-c", override), "", nil
	}
	detected, ok := detect()
	if !ok {
		return nil, "", ErrNoCaptureTool
	}
	native := ""
	if toolName, rest, found := strings.Cut(deviceID, ":"); found && toolName == detected.name && detected.list != nil {
		native = rest
	}
	used := ""
	if native != "" {
		used = deviceID
	}
	return exec.CommandContext(ctx, detected.bin, detected.args(native)...), used, nil
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
		if strings.Contains(line, "AVFoundation audio devices") {
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
