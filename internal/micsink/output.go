package micsink

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/output"
)

var outputClientConfig = "/etc/pulse/client.conf.d/00-fleet-mic.conf"

// A sound server alone is not sufficient: applications must discover it.
// Images may already contain PulseAudio without fleet's routing configuration;
// failing before ready makes the daemon run the shared audio setup script.
func outputConfigured() bool {
	data, err := os.ReadFile(outputClientConfig)
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
		// Also repair servers started by older fleet binaries. Set this before
		// choosing the output sink so its monitor never becomes the default.
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

// RunOutput captures the sink's monitor: ready\n, followed by raw wire PCM.
// It deliberately does not record fleetmic. Stdin EOF is the daemon lifeline
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
	_, err = io.Copy(stdout, pipe)
	return err
}
