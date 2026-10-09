package micsink

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func routingPulse(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"pulseaudio", "pactl", "paplay", "parec"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s unavailable", bin)
		}
	}
	oldDir, oldConfig := dir, outputClientConfig
	dir = t.TempDir()
	outputClientConfig = filepath.Join(dir, "client.conf")
	t.Cleanup(func() {
		_ = stopServer()
		dir, outputClientConfig = oldDir, oldConfig
	})
	if err := os.WriteFile(outputClientConfig, []byte("default-server = unix:"+SocketPath+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func routingPactl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := pactl(context.Background(), args...)
	if err != nil {
		t.Fatalf("pactl %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

func routingSourceIndex(t *testing.T, name string) string {
	t.Helper()
	for line := range strings.SplitSeq(routingPactl(t, "list", "short", "sources"), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[1] == name {
			return f[0]
		}
	}
	t.Fatalf("source %s missing", name)
	return ""
}

// Start the production output recorder and keep draining PCM so its stream
// really attaches. The ready greeting alone precedes parec's connection.
func routingOutput(t *testing.T) (string, *routingPCM, func()) {
	t.Helper()
	if err := EnsureOutput(); err != nil {
		t.Fatal(err)
	}
	before := make(map[string]bool)
	for line := range strings.SplitSeq(routingPactl(t, "list", "short", "source-outputs"), "\n") {
		if f := strings.Fields(line); len(f) > 1 {
			before[f[0]] = true
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := RunOutput(ctx, inR, outW)
		outW.Close()
		done <- err
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		inW.Close() // daemon disconnect, not context cancellation
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("output recorder ignored stdin EOF")
		}
		cancel()
		inR.Close()
		outR.Close()
	}
	t.Cleanup(stop)
	reader := bufio.NewReader(outR)
	if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("output greeting %q: %v", line, err)
	}
	pcm := &routingPCM{}
	go func() { _, _ = io.Copy(pcm, reader) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for line := range strings.SplitSeq(routingPactl(t, "list", "short", "source-outputs"), "\n") {
			if f := strings.Fields(line); len(f) > 1 && !before[f[0]] {
				return f[0], pcm, stop
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("output recorder never attached")
	return "", pcm, stop
}

// Keep the most recent second of stereo PCM. Tests inspect whole samples under
// a lock while the production recorder continues streaming independently.
type routingPCM struct {
	mu   sync.Mutex
	data []byte
}

func (p *routingPCM) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.data = append(p.data, b...)
	if extra := len(p.data) - 48000*4; extra > 0 {
		p.data = p.data[extra/4*4:]
	}
	return len(b), nil
}

func (p *routingPCM) amplitude(hz, rate, channels int) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	frames := rate / 10 // whole cycles of both the 440 Hz and 1000 Hz signals
	end := len(p.data) / (channels * 2) * (channels * 2)
	if end < frames*channels*2 {
		return 0
	}
	data := p.data[end-frames*channels*2 : end]
	var re, im float64
	for i := 0; i < frames; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(data[i*channels*2:])))
		phase := 2 * math.Pi * float64(hz*i) / float64(rate)
		re += v * math.Cos(phase)
		im += v * math.Sin(phase)
	}
	return 2 * math.Hypot(re, im) / float64(frames)
}

func assertRoutingTone(t *testing.T, pcm *routingPCM, rate, channels, want, reject int) {
	t.Helper()
	// Require fresh PCM after the transition; a buffered pre-toggle tone must
	// not pass on behalf of a recorder that stopped receiving audio.
	pcm.mu.Lock()
	pcm.data = append([]byte(nil), pcm.data[len(pcm.data)-len(pcm.data)%4:]...)
	pcm.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pcm.amplitude(want, rate, channels) > 5000 && pcm.amplitude(reject, rate, channels) < 100 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("PCM amplitude: %d Hz = %.1f (want >5000), %d Hz = %.1f (want <100)",
		want, pcm.amplitude(want, rate, channels), reject, pcm.amplitude(reject, rate, channels))
}

func TestOutputPulseStopAndRestartKeepsMicrophone(t *testing.T) {
	routingPulse(t)
	stopMic := routingMic(t)
	// routingMic starts asynchronously; wait for the real device before an
	// application connects to the default source.
	if err := Ensure(); err != nil {
		t.Fatal(err)
	}
	micPCM := &routingPCM{}
	stopRecorder := routingCommand(t, nil, micPCM, "parec", "--raw", "--format=s16le",
		"--rate=16000", "--channels=1", "--latency-msec=20")
	assertRoutingDemand(t, true)
	assertRoutingTone(t, micPCM, 16000, 1, 1000, 440)
	id, pcm, disconnect := routingOutput(t)
	stopPlayback := routingCommand(t, &routingLoop{pcm: routingTone(440, 48000, 2)}, io.Discard,
		"paplay", "--raw", "--format=s16le", "--rate=48000", "--channels=2", "--latency-msec=20")
	assertRoutingOutput(t, id)
	assertRoutingTone(t, pcm, 48000, 2, 440, 1000)
	disconnect()
	if err := StopOutput(); err != nil {
		t.Fatal(err)
	}
	assertRoutingDemand(t, true)
	assertRoutingTone(t, micPCM, 16000, 1, 1000, 440)
	if present, err := outputPresent(); err != nil || present {
		t.Fatalf("disabled output still present: %v %v", present, err)
	}
	id, pcm, disconnect = routingOutput(t)
	assertRoutingOutput(t, id)
	assertRoutingTone(t, pcm, 48000, 2, 440, 1000)
	assertRoutingTone(t, micPCM, 16000, 1, 1000, 440)
	stopRecorder()
	assertRoutingDemand(t, false)
	stopMic()
	disconnect()
	stopPlayback()
	if err := Stop(); err != nil {
		t.Fatal(err)
	}
	if err := StopOutput(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for serverAnswers() != no && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if serverAnswers() != no {
		t.Fatal("disabling both directions left the sound server running")
	}
	// Reconnect after both directions and the sound server have stopped.
	id, pcm, _ = routingOutput(t)
	if err := Ensure(); err != nil {
		t.Fatal(err)
	}
	assertRoutingOutput(t, id)
	assertRoutingDemand(t, false)
	routingCommand(t, &routingLoop{pcm: routingTone(440, 48000, 2)}, io.Discard,
		"paplay", "--raw", "--format=s16le", "--rate=48000", "--channels=2", "--latency-msec=20")
	assertRoutingTone(t, pcm, 48000, 2, 440, 1000)
}

func TestOutputPulseRepairsExistingServerDefault(t *testing.T) {
	routingPulse(t)
	if err := EnsureOutput(); err != nil {
		t.Fatal(err)
	}
	// Simulate an output-only sound server left by an older Fleet binary.
	routingPactl(t, "set-default-source", "fleetoutput.monitor")
	id, _, _ := routingOutput(t)
	if err := Ensure(); err != nil {
		t.Fatal(err)
	}
	assertRoutingOutput(t, id)
	assertRoutingDemand(t, false)
}

func routingTone(hz, rate, channels int) []byte {
	pcm := make([]byte, rate/10*channels*2)
	for frame := 0; frame < rate/10; frame++ {
		v := uint16(int16(10000 * math.Sin(2*math.Pi*float64(hz*frame)/float64(rate))))
		for ch := 0; ch < channels; ch++ {
			binary.LittleEndian.PutUint16(pcm[(frame*channels+ch)*2:], v)
		}
	}
	return pcm
}

type routingLoop struct {
	pcm []byte
	pos int
}

func (r *routingLoop) Read(b []byte) (int, error) {
	n := copy(b, r.pcm[r.pos:])
	r.pos = (r.pos + n) % len(r.pcm)
	return n, nil
}

func routingCommand(t *testing.T, stdin io.Reader, stdout io.Writer, name string, args ...string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env, cmd.Stdin, cmd.Stdout = pulseEnv(), stdin, stdout
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); _ = cmd.Wait() }) }
	t.Cleanup(stop)
	return stop
}

func assertRoutingDemand(t *testing.T, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if attached, ok := recorderAttached(context.Background()); ok && attached == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("microphone demand did not become %v", want)
}

// Feed deterministic microphone PCM through the production demand gate. The
// source is simulated, but PulseAudio, its FIFO, and both recorders are real.
func routingMic(t *testing.T) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	inR, inW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, inR, io.Discard) }()
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		pcm := routingTone(1000, 16000, 1)
		for {
			select {
			case <-ticker.C:
				if _, err := inW.Write(pcm); err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			inW.Close()
			<-feedDone
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("microphone sink: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("microphone sink did not stop")
			}
			inR.Close()
		})
	}
	t.Cleanup(stop)
	return stop
}

func assertRoutingOutput(t *testing.T, id string) {
	t.Helper()
	want := routingSourceIndex(t, "fleetoutput.monitor")
	rows := routingPactl(t, "list", "short", "source-outputs")
	for line := range strings.SplitSeq(rows, "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == id {
			if f[1] != want {
				t.Fatalf("output recorder %s moved to source %s; want fleetoutput.monitor (%s)\n%s", id, f[1], want, rows)
			}
			return
		}
	}
	t.Fatalf("output recorder %s disappeared: %s", id, rows)
}

// Unlike the module-presence test, keep a live RunOutput across every change.
// PulseAudio treats an explicitly requested device that happens to be the
// default as following the default; enabling the mic used to hijack it.
func TestOutputPulseRoutingAcrossMicToggles(t *testing.T) {
	for _, micFirst := range []bool{false, true} {
		name := "output-first"
		if micFirst {
			name = "mic-first"
		}
		t.Run(name, func(t *testing.T) {
			routingPulse(t)
			if micFirst {
				if err := Ensure(); err != nil {
					t.Fatal(err)
				}
			}
			id, pcm, disconnect := routingOutput(t)
			// A normal application plays to the default sink, not an explicit
			// test-only route. Leave it running through the microphone toggles.
			routingCommand(t, &routingLoop{pcm: routingTone(440, 48000, 2)}, io.Discard,
				"paplay", "--raw", "--format=s16le", "--rate=48000", "--channels=2", "--latency-msec=20")
			for i := 0; i < 3; i++ {
				if err := Ensure(); err != nil {
					t.Fatal(err)
				}
				assertRoutingOutput(t, id)
				assertRoutingDemand(t, false)
				assertRoutingTone(t, pcm, 48000, 2, 440, 1000)
				stopMic := routingMic(t)
				micPCM := &routingPCM{}
				stopRecorder := routingCommand(t, nil, micPCM, "parec", "--raw", "--format=s16le",
					"--rate=16000", "--channels=1", "--latency-msec=20")
				assertRoutingDemand(t, true)
				assertRoutingTone(t, micPCM, 16000, 1, 1000, 440)
				assertRoutingTone(t, pcm, 48000, 2, 440, 1000)
				stopRecorder()
				assertRoutingDemand(t, false)
				stopMic()
				if err := Stop(); err != nil {
					t.Fatal(err)
				}
				if source := routingPactl(t, "get-default-source"); source != "fleetnull.monitor" {
					t.Fatalf("disabled mic left default source %s; want silent fallback", source)
				}
				assertRoutingOutput(t, id)
				assertRoutingTone(t, pcm, 48000, 2, 440, 1000)
				// A new output stream while mic is off must remain independent
				// when the microphone is enabled again.
				disconnect()
				id, pcm, disconnect = routingOutput(t)
			}
		})
	}
}
