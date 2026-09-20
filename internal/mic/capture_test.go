package mic

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type pcmCollector struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *pcmCollector) sink(pcm []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Write(pcm)
}

func (c *pcmCollector) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Len()
}

func (c *pcmCollector) string() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Stop must end a recorder that would otherwise run forever, and a deliberate
// stop is not an error.
func TestCaptureStreamsUntilStopped(t *testing.T) {
	t.Setenv(EnvCapture, "while :; do printf 'pcm!'; sleep 0.01; done")
	var got pcmCollector
	capture, err := Start("", got.sink)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "audio", func() bool { return got.len() >= 8 })

	stopped := make(chan struct{})
	go func() { capture.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	if err := capture.Err(); err != nil {
		t.Fatalf("Err after Stop = %v, want nil", err)
	}
	if !strings.HasPrefix(got.string(), "pcm!pcm!") {
		t.Fatalf("got %q", got.string())
	}
}

// A recorder that dies on its own surfaces why, stderr included.
func TestCaptureReportsARecorderFailure(t *testing.T) {
	t.Setenv(EnvCapture, "echo 'device busy' >&2; exit 3")
	capture, err := Start("", func([]byte) {})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-capture.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not end")
	}
	if err := capture.Err(); err == nil || !strings.Contains(err.Error(), "device busy") {
		t.Fatalf("Err = %v, want the recorder's stderr", err)
	}
	// Every recorder runs under an sh wrapper; the error is user-visible and
	// must point at the recorder, not at /bin/sh.
	if err := capture.Err(); strings.Contains(err.Error(), "/sh:") || !strings.Contains(err.Error(), EnvCapture) {
		t.Fatalf("Err = %v, want it to name the recorder", err)
	}
}

// A configured device that cannot be opened (unplugged) must not leave the user
// with a dead microphone: the system default is tried once.
func TestCaptureFallsBackToTheDefaultDevice(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = -D ] && { echo 'no such device' >&2; exit 1; }; done\nwhile :; do printf 'default-mic'; sleep 0.01; done\n"
	if err := os.WriteFile(filepath.Join(bin, "arecord"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvCapture, "")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	origOS := goos
	goos = "linux"
	origLook, origProbe := lookPath, runProbe
	lookPath = func(name string) (string, error) {
		if name == "arecord" {
			return filepath.Join(bin, "arecord"), nil
		}
		return "", os.ErrNotExist
	}
	// The device IS enumerated (so it passes validation) — it just cannot be
	// opened, which is what an unplug between listing and recording looks like.
	runProbe = func(string, ...string) ([]byte, error) {
		return []byte("plughw:CARD=Gone,DEV=0\n    Unplugged, USB Audio\n"), nil
	}
	resetDetectCache()
	t.Cleanup(func() { goos, lookPath, runProbe = origOS, origLook, origProbe; resetDetectCache() })

	var got pcmCollector
	notified := make(chan struct{}, 1)
	capture, err := StartNotify("alsa:plughw:CARD=Gone,DEV=0", got.sink, func() { notified <- struct{}{} })
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer capture.Stop()
	waitFor(t, "audio from the default device", func() bool { return strings.Contains(got.string(), "default-mic") })
	// "Which microphone is actually open" must not be wrong: the device was
	// valid at Start, so only the runtime fallback can have set this.
	if !capture.FellBack() {
		t.Fatal("FellBack must report a fallback that happened at RUNTIME, not only at validation")
	}
	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("the fallback callback was never called")
	}
}

// A device this machine never enumerated is not even tried; the capture says so,
// so the UI can tell the user their choice is not in effect.
func TestCaptureReportsFallingBackFromAnUnknownDevice(t *testing.T) {
	fakeHost(t, "linux", []string{"arecord"}, map[string]string{"arecord -L": "default\n"})
	_, used, err := commandArgv("alsa:plughw:CARD=Elsewhere,DEV=0")
	if err != nil || used != "" {
		t.Fatalf("used = %q, err = %v", used, err)
	}
	t.Setenv("PATH", t.TempDir()) // Start must not reach a real arecord
	capture, err := Start("alsa:plughw:CARD=Elsewhere,DEV=0", func([]byte) {})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer capture.Stop()
	if !capture.FellBack() {
		t.Fatal("FellBack should report the configured device is not in effect")
	}
}

func TestStartWithoutAToolFailsUpFront(t *testing.T) {
	fakeHost(t, "linux", nil, nil)
	if _, err := Start("", func([]byte) {}); !IsNoTool(err) {
		t.Fatalf("Start err = %v, want ErrNoCaptureTool", err)
	}
}

// The promise is that the microphone closes the moment the recorder detaches.
// An override that is a script FILE makes `sh -c` fork, so the process holding
// the microphone is fleet's grandchild — and killing only the direct child
// leaves it running, reparented to init, with the microphone open. (A loop like
// this one also shrugs off the EPIPE that would stop a simpler recorder.)
func TestStopKillsARecorderThatIsAGrandchild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "recorder.pid")
	script := filepath.Join(dir, "capture.sh")
	body := "#!/bin/sh\necho $$ > " + pidFile + "\nwhile :; do head -c 320 /dev/zero; sleep 0.02; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvCapture, script+" # a script file: sh forks, the recorder is a grandchild")

	var got pcmCollector
	capture, err := Start("", got.sink)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "audio", func() bool { return got.len() > 0 })
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("recorder pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("recorder pid %q: %v", raw, err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("setup: the recorder (pid %d) should be running: %v", pid, err)
	}

	capture.Stop()
	waitFor(t, "the recorder itself — not just its shell — to be gone", func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	})
}

// fleet cannot pass a device to an override recorder, so it puts the user's
// selection in the environment: a custom recorder can honour it, and it is what
// lets a test see which device a provider actually chose.
func TestOverrideRecorderIsToldTheSelectedDevice(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "device")
	t.Setenv(EnvCapture, `printf '%s' "$`+EnvDevice+`" > `+seen+`; while :; do printf pcm; sleep 0.01; done`)
	var got pcmCollector
	capture, err := Start("pulse:desk_mic", got.sink)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer capture.Stop()
	waitFor(t, "audio", func() bool { return got.len() > 0 })
	raw, err := os.ReadFile(seen)
	if err != nil || string(raw) != "pulse:desk_mic" {
		t.Fatalf("the recorder saw %s=%q (err %v), want the selected device", EnvDevice, raw, err)
	}
}

// …and for a detected recorder that means its own name, even when it dies with
// nothing on stderr — "/bin/sh: signal: killed" names nothing at all.
func TestCaptureErrorNamesTheDetectedRecorder(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "arecord"), []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvCapture, "")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	origOS, origLook := goos, lookPath
	goos = "linux"
	lookPath = func(name string) (string, error) {
		if name == "arecord" {
			return filepath.Join(bin, "arecord"), nil
		}
		return "", os.ErrNotExist
	}
	resetDetectCache()
	t.Cleanup(func() { goos, lookPath = origOS, origLook; resetDetectCache() })

	capture, err := Start("", func([]byte) {})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-capture.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not end")
	}
	if err := capture.Err(); err == nil || !strings.HasPrefix(err.Error(), "arecord:") {
		t.Fatalf("Err = %v, want it to start with the recorder's name", err)
	}
}

// Frames must leave the client as WHOLE samples: downstream drops whole frames,
// and a dropped odd-length frame would flip the sample phase of everything
// after it. A recorder's pipe reads split anywhere.
func TestCaptureOnlyEmitsWholeSamples(t *testing.T) {
	t.Setenv(EnvCapture, `printf abc; sleep 0.05; printf d; sleep 0.05; printf efghi; sleep 0.05; printf j; sleep 5`)
	var mu sync.Mutex
	var frames [][]byte
	capture, err := Start("", func(pcm []byte) {
		mu.Lock()
		frames = append(frames, bytes.Clone(pcm))
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Stop()
	joined := func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(bytes.Join(frames, nil))
	}
	waitFor(t, "all ten bytes", func() bool { return joined() == "abcdefghij" })
	mu.Lock()
	defer mu.Unlock()
	for _, frame := range frames {
		if len(frame)%2 != 0 {
			t.Fatalf("frame %q has odd length: %q", frame, frames)
		}
	}
}
