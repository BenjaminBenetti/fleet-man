// Package micsink is the IN-INSTANCE half of fleet's virtual microphone. It runs
// inside the container (as `fleet mic sink`, exec'd by the daemon) and turns the
// PCM the daemon pipes to its stdin into a microphone any program in the
// instance can record from.
//
// The virtual device is a private PulseAudio server whose default source is a
// module-pipe-source reading a FIFO. PulseAudio — rather than an ALSA `file`
// plugin pointed at the FIFO — because a virtual capture device needs a CLOCK:
// with no hardware behind it, ALSA's file plugin hands a recorder samples as fast
// as it asks, so a 3 s recording comes back as 16 s of stuttering silence.
// PulseAudio paces the source in real time, serves any number of recorders at
// once, resamples for consumers that want something other than 16 kHz mono, and
// — via the ALSA pulse plugin the provisioning script makes the ALSA default —
// is what `arecord`, SoX, ffmpeg and native ALSA clients all end up talking to.
//
// The sink speaks a tiny protocol with the daemon over its stdio:
//
//	stdin   raw PCM (mic.SampleRate, mono, s16le)
//	stdout  one event per line: "ready", "demand 1", "demand 0", "error <why>"
//
// "demand" is what keeps the human's real microphone closed until it is needed:
// the sink reports when a recorder attaches to (and detaches from) the virtual
// source, the daemon relays that to the client, and only then is audio captured.
package micsink

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/mic"
)

const (
	// Dir holds everything the virtual microphone needs at runtime. Under /tmp
	// because it must be creatable by an unprivileged remote user and must
	// survive nothing: a restarted container simply gets a fresh server.
	Dir = "/tmp/fleet-mic"
	// SocketPath is the PulseAudio native-protocol socket. The provisioning
	// script points /etc/pulse/client.conf.d at it, so every pulse client in the
	// instance finds this server without any environment variable.
	SocketPath = Dir + "/pulse.sock"
	// FIFOPath is the pipe-source's input: whatever is written here is what the
	// microphone "hears".
	FIFOPath = Dir + "/pcm"
	// SourceName is the pipe-source's PulseAudio name.
	SourceName = mic.VirtualSourceName

	// serverStartTimeout bounds how long Ensure waits for a freshly-started
	// server to answer.
	serverStartTimeout = 5 * time.Second
)

// The sink's stdout protocol: one event per line, "<event>[ <detail>]". The
// daemon (internal/server/mic.go) parses exactly these, so they are constants
// both sides share rather than literals each side types.
const (
	EventReady  = "ready"  // the virtual microphone is up; no detail
	EventDemand = "demand" // detail: DemandOn / DemandOff
	EventError  = "error"  // detail: why the sink is giving up
	DemandOn    = "1"
	DemandOff   = "0"
)

// ErrMissingDeps means the instance lacks PulseAudio. The daemon reacts by
// running the provisioning script (internal/startup's mic script).
var ErrMissingDeps = errors.New("pulseaudio is not installed")

// Seams for tests.
var (
	lookPath = exec.LookPath
	dir      = Dir
)

func path(name string) string { return filepath.Join(dir, name) }

// serverScript is the PulseAudio startup script (-n -F): nothing but the unix
// socket, a null sink (so clients that insist on an output device are happy and
// playback goes nowhere), and the pipe-source as the default source. No
// autodetection, no hardware modules, no D-Bus — there is none of that in a
// container, and each would only add a startup delay and a log full of errors.
func serverScript() string {
	return fmt.Sprintf(`load-module module-native-protocol-unix socket=%s auth-anonymous=1
load-module module-null-sink sink_name=fleetnull sink_properties=device.description=FleetNull
load-module module-pipe-source source_name=%s file=%s format=s16le rate=%d channels=%d source_properties=device.description=FleetMicrophone
set-default-source %s
set-default-sink fleetnull
`, path("pulse.sock"), SourceName, path("pcm"), mic.SampleRate, mic.Channels, SourceName)
}

// pulseEnv is the environment for every pulse process the sink runs: a private
// runtime/state dir (so this server never collides with, or is mistaken for,
// one the user runs themselves) and the explicit server address.
func pulseEnv() []string {
	return append(os.Environ(),
		// pactl's output is parsed, and its labels are gettext-translated.
		"LC_ALL=C",
		"PULSE_RUNTIME_PATH="+path("runtime"),
		"PULSE_STATE_PATH="+path("state"),
		"PULSE_SERVER=unix:"+path("pulse.sock"),
	)
}

// serverAnswers reports whether the virtual-microphone server is up at all.
func serverAnswers() bool {
	cmd := exec.Command("pactl", "info")
	cmd.Env = pulseEnv()
	return cmd.Run() == nil
}

// micPresent reports whether the running server actually carries the fleet
// microphone. A server can be up WITHOUT it: module-pipe-source refuses to load
// over a FIFO left behind by a predecessor that was SIGKILLed (a stopped
// container), and PulseAudio carries on regardless — every recorder then
// silently gets the null sink's monitor, i.e. pure silence.
func micPresent() bool {
	sources, err := pactl("list", "short", "sources")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(sources, "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[1] == SourceName {
			return true
		}
	}
	return false
}

// Ensure makes sure the instance's virtual-microphone server is running WITH
// its microphone, (re)starting it if needed. Idempotent. It is NOT a lock: two
// Ensures racing through a cold start can both decide nothing is listening, and
// the later one's cleanup below would unlink the earlier one's fresh socket.
// The callers make that unreachable in practice — the daemon runs one sink per
// instance and the provisioning script finishes before an instance is marked
// running — and the second look right before the cleanup narrows it further.
func Ensure() error {
	for _, bin := range []string{"pulseaudio", "pactl"} {
		if _, err := lookPath(bin); err != nil {
			return ErrMissingDeps
		}
	}
	if serverAnswers() {
		if micPresent() {
			return nil
		}
		// Up but microphone-less (see micPresent): replace it.
		if err := Stop(); err != nil {
			return fmt.Errorf("stop degraded pulseaudio: %w", err)
		}
		deadline := time.Now().Add(serverStartTimeout)
		for serverAnswers() && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	for _, private := range []string{path("runtime"), path("state")} {
		// PulseAudio refuses a runtime dir that others can read.
		if err := os.MkdirAll(private, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", private, err)
		}
	}
	// Leftovers of a server that died without cleaning up (container stop is a
	// SIGKILL): a stale socket makes the new server's bind fail, and a stale
	// FIFO makes module-pipe-source refuse to load. Nothing is listening — that
	// was just established — so both are safe to remove.
	if serverAnswers() && micPresent() {
		return nil // someone else brought it up while we were getting here
	}
	_ = os.Remove(path("pulse.sock"))
	_ = os.Remove(path("pcm"))
	if err := os.WriteFile(path("fleet.pa"), []byte(serverScript()), 0o644); err != nil {
		return fmt.Errorf("write server script: %w", err)
	}

	cmd := exec.Command("pulseaudio", "-n", "-F", path("fleet.pa"),
		"--daemonize=yes", "--exit-idle-time=-1", "--use-pid-file=no",
		"--log-target=file:"+path("pulse.log"))
	cmd.Env = pulseEnv()
	cmd.Dir = "/"
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("start pulseaudio: %w: %s", err, strings.TrimSpace(string(out)))
	}

	deadline := time.Now().Add(serverStartTimeout)
	for !serverAnswers() {
		if time.Now().After(deadline) {
			return fmt.Errorf("pulseaudio did not come up within %s (see %s)", serverStartTimeout, path("pulse.log"))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !micPresent() {
		return fmt.Errorf("pulseaudio started without the %s source (see %s)", SourceName, path("pulse.log"))
	}
	return nil
}

// Stop shuts the instance's virtual-microphone server down, if it is running.
// Used when the feature is turned off: recorders then find no microphone at
// all, rather than a silent one.
func Stop() error {
	if _, err := lookPath("pactl"); err != nil || !serverAnswers() {
		return nil
	}
	cmd := exec.Command("pactl", "exit")
	cmd.Env = pulseEnv()
	return cmd.Run()
}
