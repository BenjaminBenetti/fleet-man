package micsink

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// fakePulse stands in for the instance's sound server: a `pactl` on PATH whose
// answers the test scripts through files, and a real FIFO where the pipe-source
// would be.
type fakePulse struct {
	t     *testing.T
	state string // dir holding the fake's control files
	fifo  string
}

func newFakePulse(t *testing.T) *fakePulse {
	t.Helper()
	fake := &fakePulse{t: t, state: t.TempDir()}
	bin := t.TempDir()
	runtime := t.TempDir()

	script := `#!/bin/sh
state="` + fake.state + `"
case "$*" in
  info) exit 0 ;;
  "list short sources") printf '0\tfleetnull.monitor\tmodule-null-sink.c\n1\t` + SourceName + `\tmodule-pipe-source.c\n' ;;
  "list short source-outputs")
    [ -e "$state/fail" ] && exit 1
    [ -e "$state/hang" ] && exec sleep 60
    cat "$state/outputs" 2>/dev/null ;;
  subscribe) touch "$state/events"; exec tail -n 0 -f "$state/events" ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "pactl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")

	origDir, origLook := dir, lookPath
	dir = runtime
	lookPath = func(string) (string, error) { return "/stub", nil }
	t.Cleanup(func() { dir, lookPath = origDir, origLook })

	fake.fifo = filepath.Join(runtime, "pcm")
	if err := syscall.Mkfifo(fake.fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	return fake
}

// recorders sets how many recorders are attached to the fleet microphone and
// fires the subscription event a real server would.
func (f *fakePulse) recorders(n int) {
	f.t.Helper()
	outputs := ""
	for i := range n {
		outputs += string(rune('4'+i)) + "\t1\t12\tprotocol-native.c\n"
	}
	if err := os.WriteFile(filepath.Join(f.state, "outputs"), []byte(outputs), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.event()
}

func (f *fakePulse) event() {
	f.t.Helper()
	events, err := os.OpenFile(filepath.Join(f.state, "events"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	defer events.Close()
	_, _ = events.WriteString("Event 'change' on source-output #4\n")
}

func (f *fakePulse) heard() []byte { return readAvailable(f.t, f.fifo) }

// runningSink is a Run in flight: its protocol lines, and a pipe to its stdin.
type runningSink struct {
	stdin *io.PipeWriter
	lines chan string
	done  chan error
}

func startSink(t *testing.T) *runningSink {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	sink := &runningSink{stdin: stdinW, lines: make(chan string, 16), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		sink.done <- Run(ctx, stdinR, stdoutW)
		_ = stdoutW.Close()
	}()
	go func() {
		scanner := bufio.NewScanner(stdoutR)
		for scanner.Scan() {
			sink.lines <- scanner.Text()
		}
	}()
	// Runs BEFORE newFakePulse's cleanup restores the package seams (LIFO), and
	// must not return while Run can still read them.
	t.Cleanup(func() {
		cancel()
		_ = stdinW.Close()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("Run did not stop")
		}
	})
	return sink
}

func (s *runningSink) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-s.lines:
		if got != want {
			t.Fatalf("sink said %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("sink never said %q", want)
	}
}

func (s *runningSink) expectQuiet(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case got := <-s.lines:
		t.Fatalf("sink said %q, want nothing", got)
	case <-time.After(d):
	}
}

// The whole stdio protocol, end to end: the daemon parses exactly these lines
// (the Event* constants), and the privacy story rests on exactly this gating.
func TestRunSpeaksTheProtocolAndGatesAudioOnDemand(t *testing.T) {
	pulse := newFakePulse(t)
	sink := startSink(t)
	sink.expect(t, EventReady)

	// Nobody recording: audio goes nowhere.
	_, _ = sink.stdin.Write([]byte("nobody is listening."))
	time.Sleep(100 * time.Millisecond)
	if got := pulse.heard(); len(got) != 0 {
		t.Fatalf("audio reached the microphone with no recorder attached: %q", got)
	}

	pulse.recorders(1)
	sink.expect(t, EventDemand+" "+DemandOn)
	_, _ = sink.stdin.Write([]byte("hello!"))
	deadline := time.Now().Add(5 * time.Second)
	var heard []byte
	for len(heard) < 6 && time.Now().Before(deadline) {
		heard = append(heard, pulse.heard()...)
		time.Sleep(10 * time.Millisecond)
	}
	if string(heard) != "hello!" {
		t.Fatalf("the recorder heard %q, want %q", heard, "hello!")
	}

	// A second recorder changes nothing the daemon needs to hear about.
	pulse.recorders(2)
	sink.expectQuiet(t, 300*time.Millisecond)

	pulse.recorders(0)
	sink.expect(t, EventDemand+" "+DemandOff)
	_, _ = sink.stdin.Write([]byte("too late"))
	time.Sleep(100 * time.Millisecond)
	if got := pulse.heard(); len(got) != 0 {
		t.Fatalf("audio after the recorder left: %q", got)
	}

	// The daemon detaching (stdin EOF) is the normal, clean exit.
	_ = sink.stdin.Close()
	select {
	case err := <-sink.done:
		if err != nil {
			t.Fatalf("Run = %v, want nil on stdin EOF", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return on stdin EOF")
	}
}

// A probe that ERRORS says nothing. Reading it as "nobody is recording" would
// close the microphone mid-sentence and drain audio a still-attached recorder
// was about to read.
func TestRunTreatsAFailedProbeAsUnchanged(t *testing.T) {
	pulse := newFakePulse(t)
	sink := startSink(t)
	sink.expect(t, EventReady)
	pulse.recorders(1)
	sink.expect(t, EventDemand+" "+DemandOn)

	if err := os.WriteFile(filepath.Join(pulse.state, "fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	pulse.event()
	sink.expectQuiet(t, 500*time.Millisecond)

	_, _ = sink.stdin.Write([]byte("still talking"))
	deadline := time.Now().Add(5 * time.Second)
	var heard []byte
	for len(heard) == 0 && time.Now().Before(deadline) {
		heard = pulse.heard()
		time.Sleep(10 * time.Millisecond)
	}
	if len(heard) == 0 {
		t.Fatal("a failed probe closed the microphone")
	}
}

func TestRunReportsMissingDeps(t *testing.T) {
	origLook := lookPath
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { lookPath = origLook })
	sink := startSink(t)
	sink.expect(t, EventError+" "+ErrMissingDeps.Error())
}

// This is the privacy gate, so it must fail CLOSED. One failed probe is a blip
// ("unchanged"); a server that keeps the subscription alive while never
// answering — here: pactl HANGS — must not hold the human's microphone open,
// and must not be able to wedge the sink's shutdown either.
func TestRunFailsClosedWhenTheServerStopsAnswering(t *testing.T) {
	origTimeout := pactlTimeout
	pactlTimeout = 150 * time.Millisecond
	t.Cleanup(func() { pactlTimeout = origTimeout })

	pulse := newFakePulse(t)
	sink := startSink(t)
	sink.expect(t, EventReady)
	pulse.recorders(1)
	sink.expect(t, EventDemand+" "+DemandOn)

	if err := os.WriteFile(filepath.Join(pulse.state, "hang"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for range maxBlindRecounts {
		pulse.event()
		time.Sleep(pactlTimeout + 200*time.Millisecond)
	}
	sink.expect(t, EventDemand+" "+DemandOff)

	// …and with pactl still hanging, the sink must still be able to exit.
	_ = sink.stdin.Close()
	select {
	case <-sink.done:
	case <-time.After(10 * time.Second):
		t.Fatal("a hung pactl wedged the sink's shutdown")
	}
}
