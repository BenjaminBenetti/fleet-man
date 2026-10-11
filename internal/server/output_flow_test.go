package server

import (
	"context"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/output"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// level is one output frame of a constant sample value.
func level(v int16) []byte {
	frame := make([]byte, output.ChunkBytes)
	for i := 0; i < len(frame); i += 2 {
		binary.LittleEndian.PutUint16(frame[i:], uint16(v))
	}
	return frame
}

// serveFake serves one fake source through the hub until the test ends,
// returning the client the hub routes to and the source's fake exec.
func serveFake(t *testing.T) (*outputHub, *outputClient, *fakeMicSink, chan error) {
	t.Helper()
	original := openOutputSource
	t.Cleanup(func() { openOutputSource = original })
	fake := newFakeMicSink()
	openOutputSource = func(*fleet.Instance) (io.ReadWriteCloser, bool, error) { return fake, true, nil }
	h := newOutputHub()
	h.configure(state.OutputSettings{Enabled: true})
	c := h.add("desk")
	takeOutput(t, c)
	s := &outputSource{inst: &fleet.Instance{Name: "i"}}
	h.sources["f/i"] = s
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := h.serveSource(ctx, "f/i", s)
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the source did not stop")
		}
	})
	return h, c, fake, done
}

// A source's leading silence is not routed (it would start the client's
// player for nothing); a burst is, with at most outputQuietFrames of silence
// after it; dither counts as silence.
func TestOutputRoutesBurstsWithoutLeadingSilence(t *testing.T) {
	_, c, fake, _ := serveFake(t)
	go func() {
		fake.emit("ready")
		for range 10 {
			_, _ = fake.eventsW.Write(level(0))
		}
		_, _ = fake.eventsW.Write(level(4000))
		for range 10 {
			_, _ = fake.eventsW.Write(level(3)) // dither
		}
		_, _ = fake.eventsW.Write(level(-4000))
	}()
	var loud []bool
	for len(loud) < 1+outputQuietFrames+1 {
		audio := takeOutput(t, c).GetAudio()
		loud = append(loud, !output.Silent(audio.GetPcm()))
	}
	want := []bool{true, false, false, false, false, false, true}
	for i := range want {
		if loud[i] != want[i] {
			t.Fatalf("routed %v, want %v", loud, want)
		}
	}
	select {
	case msg := <-c.frames:
		t.Fatalf("routed more than the burst and its tail: %v", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

// A source that goes silent on the wire (a dead path; a live quiet one sends
// a silent frame every micsink.AliveInterval) is closed after the deadline.
func TestOutputSourceThatSendsNothingIsClosed(t *testing.T) {
	orig := outputAliveDeadline
	outputAliveDeadline = 150 * time.Millisecond
	t.Cleanup(func() { outputAliveDeadline = orig })
	_, _, fake, done := serveFake(t)
	fake.emit("ready")
	for range 4 { // heartbeats keep it open
		time.Sleep(outputAliveDeadline / 2)
		_, _ = fake.eventsW.Write(level(0))
	}
	if fake.isClosed() {
		t.Fatal("a source with heartbeats was closed")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the dead source ended without an error")
		}
		done <- err // for the cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("a source that sent nothing was not closed")
	}
	if !fake.isClosed() {
		t.Fatal("the dead source's exec was not closed")
	}
}

// Audible frames, and audible frames arriving late, reach the instance's audio
// flow; silence never does (an open but silent stream must not pace a pod's
// bulk transfers).
func TestOutputFeedsTheInstancesAudioFlow(t *testing.T) {
	flow := &recordedFlow{}
	withAudioFlow(t, flow)
	t.Setenv(EnvAudioLateThreshold, "1ms")
	_, c, fake, _ := serveFake(t)
	fake.emit("ready")
	for range 20 {
		_, _ = fake.eventsW.Write(level(0))
	}
	time.Sleep(50 * time.Millisecond)
	if _, audible, late := flow.snapshot(); audible != 0 || late != 0 {
		t.Fatalf("silence counted: audible %d, late %d", audible, late)
	}
	for range 4 {
		_, _ = fake.eventsW.Write(level(5000))
		takeOutput(t, c)
		time.Sleep(3 * outputFrameInterval) // each frame well behind its schedule
	}
	eventually(t, "audible and late frames to reach the flow", func() bool {
		_, audible, late := flow.snapshot()
		return audible == 4 && late >= 2
	})
}

func TestAudioLateThreshold(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{{"", 0}, {"5ms", 5 * time.Millisecond}, {"nonsense", 0}, {"-1s", 0}} {
		t.Setenv(EnvAudioLateThreshold, tc.env)
		if got := audioLateThreshold(); got != tc.want {
			t.Errorf("%s=%q: %v, want %v", EnvAudioLateThreshold, tc.env, got, tc.want)
		}
	}
}
