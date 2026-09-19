package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/mic"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeMicSink stands in for a running `fleet mic sink`: the test scripts its
// stdout event lines with emit and inspects what the hub wrote to its stdin.
type fakeMicSink struct {
	eventsR *io.PipeReader
	eventsW *io.PipeWriter

	mu     sync.Mutex
	stdin  bytes.Buffer
	closed bool
}

func newFakeMicSink() *fakeMicSink {
	r, w := io.Pipe()
	return &fakeMicSink{eventsR: r, eventsW: w}
}

func (f *fakeMicSink) Read(p []byte) (int, error) { return f.eventsR.Read(p) }

func (f *fakeMicSink) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	return f.stdin.Write(p)
}

func (f *fakeMicSink) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return f.eventsW.Close()
}

func (f *fakeMicSink) emit(line string) { _, _ = fmt.Fprintln(f.eventsW, line) }

func (f *fakeMicSink) received() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return bytes.Clone(f.stdin.Bytes())
}

func (f *fakeMicSink) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// micHarness is a test server with the mic hub's sync loop running and the
// container layer stubbed.
type micHarness struct {
	svc    *service
	client fleetgrpc.FleetServiceClient

	mu         sync.Mutex
	opens      []*fakeMicSink // one per openMicSink call, in order
	prepares   int
	prepareErr error // what prepareMicInstance returns
	stops      []string
	// script decides what each successive open yields; nil = a healthy sink.
	script func(open int) (sink *fakeMicSink, supported bool, err error)
	opened chan *fakeMicSink
}

func newMicHarness(t *testing.T, enabled bool) *micHarness {
	t.Helper()
	isolateFleetDir(t)
	seedForwardInstance(t)
	if err := state.SaveConfig(&state.Config{MicSettings: state.MicSettings{Enabled: enabled}}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	h := &micHarness{opened: make(chan *fakeMicSink, 16)}
	origOpen, origPrepare, origStop := openMicSink, prepareMicInstance, stopMicServer
	openMicSink = func(*fleet.Instance) (micSinkConn, bool, error) {
		h.mu.Lock()
		index := len(h.opens)
		script := h.script
		h.mu.Unlock()
		sink, supported, err := newFakeMicSink(), true, error(nil)
		if script != nil {
			sink, supported, err = script(index)
		}
		h.mu.Lock()
		h.opens = append(h.opens, sink)
		h.mu.Unlock()
		if !supported || err != nil {
			return nil, supported, err
		}
		h.opened <- sink
		return sink, true, nil
	}
	prepareMicInstance = func(*fleet.Instance) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.prepares++
		return h.prepareErr
	}
	stopMicServer = func(inst *fleet.Instance) {
		h.mu.Lock()
		h.stops = append(h.stops, inst.Name)
		h.mu.Unlock()
	}
	t.Cleanup(func() { openMicSink, prepareMicInstance, stopMicServer = origOpen, origPrepare, origStop })

	svc, client, cleanup := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	go svc.mic.run(ctx)
	t.Cleanup(func() { cancel(); cleanup() })
	h.svc, h.client = svc, client
	return h
}

func (h *micHarness) nextSink(t *testing.T) *fakeMicSink {
	t.Helper()
	select {
	case sink := <-h.opened:
		return sink
	case <-time.After(5 * time.Second):
		t.Fatal("no sink was opened")
		return nil
	}
}

// micStream is a provider stream with its demand frames pumped onto a channel.
type micStream struct {
	stream  fleetgrpc.FleetService_MicClient
	demands chan *fleetgrpc.MicDemand
	done    chan error
}

func openMicStream(t *testing.T, client fleetgrpc.FleetServiceClient) *micStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := client.Mic(ctx)
	if err != nil {
		t.Fatalf("Mic: %v", err)
	}
	if err := stream.Send(&fleetgrpc.MicUp{Msg: &fleetgrpc.MicUp_Open{Open: &fleetgrpc.MicOpen{
		SampleRate: mic.SampleRate, Channels: mic.Channels,
	}}}); err != nil {
		t.Fatalf("send open: %v", err)
	}
	ms := &micStream{stream: stream, demands: make(chan *fleetgrpc.MicDemand, 16), done: make(chan error, 1)}
	go func() {
		for {
			down, err := stream.Recv()
			if err != nil {
				ms.done <- err
				return
			}
			if demand := down.GetDemand(); demand != nil {
				ms.demands <- demand
			}
		}
	}()
	return ms
}

func (ms *micStream) expectDemand(t *testing.T, active bool, instances ...string) {
	t.Helper()
	select {
	case demand := <-ms.demands:
		if demand.GetActive() != active || fmt.Sprint(demand.GetInstances()) != fmt.Sprint(instances) {
			t.Fatalf("demand = active:%v %v, want active:%v %v", demand.GetActive(), demand.GetInstances(), active, instances)
		}
	case err := <-ms.done:
		t.Fatalf("stream ended while waiting for demand: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatalf("no demand frame (want active:%v %v)", active, instances)
	}
}

func (ms *micStream) sendAudio(t *testing.T, pcm []byte) {
	t.Helper()
	if err := ms.stream.Send(&fleetgrpc.MicUp{Msg: &fleetgrpc.MicUp_Audio{Audio: pcm}}); err != nil {
		t.Fatalf("send audio: %v", err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestMicDemandGatesAudio is the core contract: the provider hears about demand
// the moment a sink reports it, audio reaches exactly the demanding sink, and
// audio sent with no demand goes nowhere.
func TestMicDemandGatesAudio(t *testing.T) {
	h := newMicHarness(t, true)
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false) // the greeting: attached, nobody recording

	sink := h.nextSink(t)
	sink.emit("ready")

	sink.emit("demand 1")
	provider.expectDemand(t, true, "alpha/i1")
	provider.sendAudio(t, []byte("hello "))
	provider.sendAudio(t, []byte("world"))
	eventually(t, "audio to reach the sink", func() bool { return string(sink.received()) == "hello world" })

	sink.emit("demand 0")
	provider.expectDemand(t, false)
	provider.sendAudio(t, []byte("late!"))
	// Give a wrongly-routed frame time to land before asserting it did not.
	time.Sleep(50 * time.Millisecond)
	if got := string(sink.received()); got != "hello world" {
		t.Fatalf("sink received %q; audio outside demand must be dropped", got)
	}
}

func TestMicRejectsWhenDisabled(t *testing.T) {
	h := newMicHarness(t, false)
	provider := openMicStream(t, h.client)
	select {
	case err := <-provider.done:
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("err = %v, want FailedPrecondition", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a disabled daemon must refuse the stream")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.opens) != 0 {
		t.Fatalf("no sink may be opened while disabled; got %d", len(h.opens))
	}
}

func TestMicRejectsBadOpen(t *testing.T) {
	h := newMicHarness(t, true)
	for name, first := range map[string]*fleetgrpc.MicUp{
		"audio before open": {Msg: &fleetgrpc.MicUp_Audio{Audio: []byte("x")}},
		"wrong format": {Msg: &fleetgrpc.MicUp_Open{Open: &fleetgrpc.MicOpen{
			SampleRate: 48000, Channels: 2,
		}}},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stream, err := h.client.Mic(ctx)
		if err != nil {
			t.Fatalf("%s: Mic: %v", name, err)
		}
		_ = stream.Send(first)
		if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: err = %v, want InvalidArgument", name, err)
		}
		cancel()
	}
}

// TestMicNewestProviderIsActive: a second TUI supersedes the first without
// ending it, its audio is the only audio routed, and the first is promoted back
// when the second leaves.
func TestMicNewestProviderIsActive(t *testing.T) {
	h := newMicHarness(t, true)
	first := openMicStream(t, h.client)
	first.expectDemand(t, false)
	sink := h.nextSink(t)
	sink.emit("ready")
	sink.emit("demand 1")
	first.expectDemand(t, true, "alpha/i1")

	second := openMicStream(t, h.client)
	second.expectDemand(t, true, "alpha/i1") // greeted with the live demand
	first.expectDemand(t, false)             // told to close its microphone

	first.sendAudio(t, []byte("stale"))
	second.sendAudio(t, []byte("fresh"))
	eventually(t, "the active provider's audio", func() bool { return string(sink.received()) == "fresh" })

	if err := second.stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	first.expectDemand(t, true, "alpha/i1") // promoted
}

// TestMicDisableTearsEverythingDown: turning the feature off through SetConfig
// ends provider streams, detaches sinks and stops the instances' sound servers.
func TestMicDisableTearsEverythingDown(t *testing.T) {
	h := newMicHarness(t, true)
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)
	sink := h.nextSink(t)
	sink.emit("ready")

	config, err := state.LoadConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	config.MicSettings.Enabled = false
	if _, err := h.client.SetConfig(context.Background(), &fleetgrpc.SetConfigRequest{Config: protoconv.ConfigToProto(config)}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	select {
	case err := <-provider.done:
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("provider ended with %v, want FailedPrecondition", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider stream must end when the feature is turned off")
	}
	eventually(t, "the sink to be closed", sink.isClosed)
	eventually(t, "the instance's sound server to be stopped", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.stops) == 1 && h.stops[0] == "i1"
	})
}

// TestMicSinksLeaveWithTheLastProvider: no provider, nothing injected.
func TestMicSinksLeaveWithTheLastProvider(t *testing.T) {
	h := newMicHarness(t, true)
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)
	sink := h.nextSink(t)
	sink.emit("ready")

	if err := provider.stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	eventually(t, "the sink to be closed", sink.isClosed)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.stops) != 0 {
		t.Fatalf("a provider leaving must not stop sound servers (that is for disable); got %v", h.stops)
	}
}

// TestMicPreparesAnInstanceOnce: a sink that cannot start (an instance created
// before the feature was on) triggers the lazy install exactly once, then
// attaches.
func TestMicPreparesAnInstanceOnce(t *testing.T) {
	h := newMicHarness(t, true)
	h.script = func(open int) (*fakeMicSink, bool, error) {
		sink := newFakeMicSink()
		if open == 0 {
			go func() {
				sink.emit("error pulseaudio is not installed")
				_ = sink.Close()
			}()
		}
		return sink, true, nil
	}
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)

	h.nextSink(t) // the broken one
	healthy := h.nextSink(t)
	healthy.emit("ready")
	healthy.emit("demand 1")
	provider.expectDemand(t, true, "alpha/i1")

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.prepares != 1 {
		t.Fatalf("prepare ran %d times, want exactly 1", h.prepares)
	}
}

// brokenSink is a sink that reports an error and exits without ever being ready.
func brokenSink() *fakeMicSink {
	sink := newFakeMicSink()
	go func() {
		sink.emit("error pulseaudio is not installed")
		_ = sink.Close()
	}()
	return sink
}

// TestMicPrepareFailureStillRetriesTheSink: the install script can fail on its
// LAST step (an image whose own asound.conf bypasses PulseAudio) with a working
// sound server installed — so a failed prepare is surfaced but the sink is
// still retried, and comes up.
func TestMicPrepareFailureStillRetriesTheSink(t *testing.T) {
	h := newMicHarness(t, true)
	h.prepareErr = fmt.Errorf("mic: exit status 3")
	h.script = func(open int) (*fakeMicSink, bool, error) {
		if open == 0 {
			return brokenSink(), true, nil
		}
		return newFakeMicSink(), true, nil
	}
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)

	h.nextSink(t)
	healthy := h.nextSink(t)
	healthy.emit("ready")
	healthy.emit("demand 1")
	provider.expectDemand(t, true, "alpha/i1")
}

// TestMicDoesNotReprepareInAHurry: an instance that can never be prepared must
// not have apt run against it on every attach — but the back-off is a window,
// not "once per daemon lifetime": one bad minute must not be permanent.
func TestMicDoesNotReprepareInAHurry(t *testing.T) {
	h := newMicHarness(t, true)
	h.script = func(int) (*fakeMicSink, bool, error) { return brokenSink(), true, nil }
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)

	h.nextSink(t)
	h.nextSink(t) // the immediate post-prepare retry; fails too → backed off
	eventually(t, "the instance to be backed off", func() bool {
		h.svc.mic.mu.Lock()
		defer h.svc.mic.mu.Unlock()
		_, backedOff := h.svc.mic.retryAt["alpha/i1"]
		return backedOff
	})
	h.mu.Lock()
	if h.prepares != 1 {
		t.Fatalf("prepare ran %d times, want 1", h.prepares)
	}
	h.mu.Unlock()

	// Age the attempt past the window and lift the attach back-off: the next
	// failure prepares again.
	h.svc.mic.mu.Lock()
	h.svc.mic.preparedAt["c1"] = time.Now().Add(-micPrepareRetry - time.Minute)
	delete(h.svc.mic.retryAt, "alpha/i1")
	h.svc.mic.mu.Unlock()
	h.svc.mic.poke()
	eventually(t, "a second prepare once the window has passed", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.prepares == 2
	})
}

// TestMicSkipsUnsupportedBackends: a backend with no sink is not an error and
// must never trigger the package install.
func TestMicSkipsUnsupportedBackends(t *testing.T) {
	h := newMicHarness(t, true)
	h.script = func(int) (*fakeMicSink, bool, error) { return nil, false, nil }
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)

	eventually(t, "the hub to try the instance", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.opens) == 1
	})
	time.Sleep(50 * time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.prepares != 0 {
		t.Fatalf("prepare ran %d times for an unsupported backend", h.prepares)
	}
	if len(h.opens) != 1 {
		t.Fatalf("an unsupported instance must be backed off, got %d opens", len(h.opens))
	}
}
