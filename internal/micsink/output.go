package micsink

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/output"
)

var outputClientConfig = "/etc/pulse/client.conf.d/00-fleet-mic.conf"

// UserClientConfig is where the audio setup script points the session user's
// pulse clients at the private server when the instance has no root to write
// /etc (a cluster that runs every container as one non-root uid, on an image
// that ships the audio packages), relative to the user's home.
const UserClientConfig = ".config/pulse/client.conf.d/00-fleet-mic.conf"

// userClientConfig is UserClientConfig under the session user's home ("" when
// there is none); a var for tests.
var userClientConfig = func() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, UserClientConfig)
}

// A sound server alone is not sufficient: applications must discover it.
// Images may already contain PulseAudio without fleet's routing configuration;
// failing before ready makes the daemon run the shared audio setup script.
// The system-wide drop-in serves every user; the session user's own (written
// where there is no root) serves that user, who is the only one there is.
func outputConfigured() bool {
	for _, path := range []string{outputClientConfig, userClientConfig()} {
		if path != "" && pointsAtServer(path) {
			return true
		}
	}
	return false
}

// pointsAtServer reports whether the pulse client config at path sends its
// clients to the private server.
func pointsAtServer(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "default-server" && strings.TrimSpace(value) == "unix:"+SocketPath {
			return true
		}
	}
	return false
}

// Both directions share the sound server, but own separate modules. A file
// lock serializes cold starts and module changes across the two fleet execs.
func withAudioLock(fn func() error) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(path("audio.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	deadline := time.Now().Add(20 * time.Second)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK || time.Now().After(deadline) {
			return fmt.Errorf("lock audio server: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func outputPresent() (bool, error) {
	sinks, err := pactl(context.Background(), "list", "short", "sinks")
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(sinks, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[1] == output.VirtualSink {
			return true, nil
		}
	}
	return false, nil
}

// EnsureOutput installs a clocked virtual sink without creating a microphone.
func EnsureOutput() error {
	return withAudioLock(func() error {
		if err := ensureAudio(false); err != nil {
			return err
		}
		// Also prepare servers left by older fleet binaries for a NEW recorder;
		// an old recorder already following the default must be restarted.
		// Set this before choosing the output sink so its monitor never becomes
		// the default.
		switch micPresent() {
		case no:
			if _, err := pactl(context.Background(), "set-default-source", silentSource); err != nil {
				return err
			}
		case unknown:
			return fmt.Errorf("cannot determine the microphone source; leaving audio routing alone")
		}
		present, err := outputPresent()
		if err != nil {
			return err
		}
		if !present {
			if _, err := pactl(context.Background(), "load-module", "module-null-sink", "sink_name="+output.VirtualSink, "format=s16le", "rate=48000", "channels=2", "sink_properties=device.description=FleetAudioOutput"); err != nil {
				return err
			}
		}
		_, err = pactl(context.Background(), "set-default-sink", output.VirtualSink)
		return err
	})
}

func StopOutput() error {
	return withAudioLock(func() error {
		if _, err := lookPath("pactl"); err != nil || serverAnswers() == no {
			return nil
		}
		if micPresent() == no {
			return stopServer()
		}
		// Keep the input server and remove only this sink's module by ID.
		modules, err := pactl(context.Background(), "list", "short", "modules")
		if err != nil {
			return err
		}
		for line := range strings.SplitSeq(modules, "\n") {
			f := strings.Fields(line)
			if len(f) > 2 && f[1] == "module-null-sink" {
				for _, arg := range f[2:] {
					if arg == "sink_name="+output.VirtualSink {
						_, _ = pactl(context.Background(), "set-default-sink", "fleetnull")
						_, err = pactl(context.Background(), "unload-module", f[0])
						return err
					}
				}
			}
		}
		return nil
	})
}

// The output source's gating. Nothing goes out while the instance plays
// silence: an output stream is open to every running instance while a client
// is attached (audio output is on by default), and on a cluster each one
// would otherwise cost 192 KB/s through the API server. An audible burst goes
// out whole, followed by outputTail silent frames (so the client's player
// ends on silence rather than mid-sample), then nothing; while it is quiet, a
// silent frame every AliveInterval tells the daemon the source still runs
// (the daemon drops it, and closes a source it has heard nothing from for
// AliveDeadline). The null sink's monitor is exactly silent when nothing
// plays into it, so this also gates the stream on there being a sink input.
const outputTail = 5

// outputAliveInterval is how often a quiet source sends its heartbeat frame
// (a var so tests can shorten it).
var outputAliveInterval = AliveInterval

// RunOutput captures the sink's monitor: ready\n, followed by raw wire PCM in
// whole frames of output.ChunkBytes, gated on audibility (outputTail). It
// deliberately does not record fleetmic. Stdin EOF is the daemon lifeline
// (docker exec killing its local process alone need not kill the remote exec).
func RunOutput(parent context.Context, stdin io.Reader, stdout io.Writer) error {
	if !outputConfigured() {
		return fmt.Errorf("instance audio routing is not configured")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, stdin); cancel() }()
	if err := EnsureOutput(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "parec", "--raw", "--format=s16le", "--rate=48000", "--channels=2", "--latency-msec=20", "--device="+output.VirtualSink+".monitor")
	cmd.Env = pulseEnv()
	cmd.WaitDelay = time.Second
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	if _, err := fmt.Fprintln(stdout, "ready"); err != nil {
		return err
	}
	return forwardOutput(ctx, pipe, stdout)
}

// forwardOutput sends the audible frames of pcm (and their tail) to out, and
// a silent heartbeat frame while it is quiet, until pcm or out fails or ctx
// ends. Every write is one or more whole frames.
func forwardOutput(ctx context.Context, pcm io.Reader, out io.Writer) error {
	var mu sync.Mutex
	last := time.Now()
	send := func(frame []byte) error {
		mu.Lock()
		defer mu.Unlock()
		last = time.Now()
		_, err := out.Write(frame)
		return err
	}
	stop := make(chan struct{})
	defer close(stop)
	beatErr := make(chan error, 1)
	go func() {
		silence := make([]byte, output.ChunkBytes)
		tick := time.NewTicker(max(outputAliveInterval/5, 10*time.Millisecond))
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			mu.Lock()
			due := time.Since(last) >= outputAliveInterval
			mu.Unlock()
			if due {
				if err := send(silence); err != nil {
					beatErr <- err
					return
				}
			}
		}
	}()
	// A source starts quiet: leading silence is not sent.
	quiet := outputTail
	for {
		frame := make([]byte, output.ChunkBytes)
		if _, err := io.ReadFull(pcm, frame); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case err := <-beatErr:
			return err
		default:
		}
		if output.Silent(frame) {
			if quiet >= outputTail {
				continue
			}
			quiet++
		} else {
			quiet = 0
		}
		if err := send(frame); err != nil {
			return err
		}
	}
}
