package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/mic"
	"github.com/BenjaminBenetti/fleet-man/internal/micsink"
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
	stopped := make(chan struct{})
	go func() { defer close(stopped); svc.mic.run(ctx) }()
	// Wait for the sync loop to be gone before earlier-registered cleanups
	// restore the package seams it reads.
	t.Cleanup(func() { cancel(); <-stopped; cleanup() })
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

// seedMicInstances replaces the seeded state with the named running instances
// (container id = "c-<name>").
func seedMicInstances(t *testing.T, names ...string) {
	t.Helper()
	var instances []*fleet.Instance
	for _, name := range names {
		instances = append(instances, &fleet.Instance{
			Name: name, Backend: fleet.BackendDevcontainer, WorkspaceDir: "/ws/alpha/" + name,
			ContainerID: "c-" + name, Status: fleet.StatusRunning,
		})
	}
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: instances}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestMicRoutesOnlyToDemandingInstances: demand is aggregated across instances
// (the provider is told exactly who is listening, and only goes idle when the
// LAST one stops), and audio fans out to the instances that asked — not to a
// neighbour that merely has a sink.
func TestMicRoutesOnlyToDemandingInstances(t *testing.T) {
	h := newMicHarness(t, true)
	seedMicInstances(t, "i1", "i2")
	sinks := map[string]*fakeMicSink{}
	var sinksMu sync.Mutex
	origOpen := openMicSink
	openMicSink = func(inst *fleet.Instance) (micSinkConn, bool, error) {
		sink := newFakeMicSink()
		sinksMu.Lock()
		sinks[inst.Name] = sink
		sinksMu.Unlock()
		return sink, true, nil
	}
	t.Cleanup(func() { openMicSink = origOpen })

	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)
	eventually(t, "both sinks to open", func() bool { sinksMu.Lock(); defer sinksMu.Unlock(); return len(sinks) == 2 })
	sinksMu.Lock()
	one, two := sinks["i1"], sinks["i2"]
	sinksMu.Unlock()
	one.emit("ready")
	two.emit("ready")

	one.emit("demand 1")
	provider.expectDemand(t, true, "alpha/i1")
	provider.sendAudio(t, []byte("for-one "))
	eventually(t, "i1 to hear it", func() bool { return string(one.received()) == "for-one " })

	two.emit("demand 1")
	provider.expectDemand(t, true, "alpha/i1", "alpha/i2")
	provider.sendAudio(t, []byte("for-both"))
	eventually(t, "both to hear it", func() bool {
		return string(one.received()) == "for-one for-both" && string(two.received()) == "for-both"
	})

	one.emit("demand 0")
	provider.expectDemand(t, true, "alpha/i2") // still live: i2 is listening
	two.emit("demand 0")
	provider.expectDemand(t, false)
}

// TestMicAttachUsesTheSinkItWasGiven is the regression test for two attach
// goroutines serving ONE sink: attach used to look the key up, so a drop +
// re-insert between sync's unlock and the goroutine running gave both the same
// *micSink — two processes in the container, the first never closed.
func TestMicAttachUsesTheSinkItWasGiven(t *testing.T) {
	h := newMicHarness(t, true)
	hub := newMicHub()
	inst := &fleet.Instance{Name: "i1", ContainerID: "c1"}
	stale := &micSink{inst: inst, containerID: "c1"}
	current := &micSink{inst: inst, containerID: "c1"}
	hub.mu.Lock()
	hub.sinks["alpha/i1"] = current // what a later sync inserted for the same key
	hub.mu.Unlock()

	hub.attach("alpha/i1", stale) // the goroutine launched for the EARLIER entry
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.opens) != 0 {
		t.Fatalf("a superseded attach opened %d sink process(es) for a sink that is no longer current", len(h.opens))
	}
}

// TestMicProviderDeathReleasesTheSinks: a provider that vanishes (TUI killed,
// network gone) — not a polite CloseSend — must still count as leaving.
func TestMicProviderDeathReleasesTheSinks(t *testing.T) {
	h := newMicHarness(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := h.client.Mic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&fleetgrpc.MicUp{Msg: &fleetgrpc.MicUp_Open{Open: &fleetgrpc.MicOpen{SampleRate: mic.SampleRate, Channels: mic.Channels}}}); err != nil {
		t.Fatal(err)
	}
	sink := h.nextSink(t)
	sink.emit("ready")
	cancel() // die
	eventually(t, "the sink to be closed after the provider died", sink.isClosed)
	eventually(t, "the provider to be forgotten", func() bool {
		h.svc.mic.mu.Lock()
		defer h.svc.mic.mu.Unlock()
		return len(h.svc.mic.providers) == 0
	})
}

// TestMicSinkThatDiesAfterReadyComesBackQuickly: a working sink that dies on
// its own is a restarting container, not a broken image — quick back-off, and
// no package install.
func TestMicSinkThatDiesAfterReadyComesBackQuickly(t *testing.T) {
	h := newMicHarness(t, true)
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)
	first := h.nextSink(t)
	first.emit("ready")
	first.emit("demand 1")
	provider.expectDemand(t, true, "alpha/i1")

	_ = first.Close()               // the process dies under us
	provider.expectDemand(t, false) // its demand goes with it
	h.svc.mic.mu.Lock()
	at, backedOff := h.svc.mic.retryAt["alpha/i1"]
	h.svc.mic.mu.Unlock()
	if !backedOff || time.Until(at) > micRetryQuick+time.Second {
		t.Fatalf("want a quick (%s) back-off, got %v (set=%v)", micRetryQuick, time.Until(at), backedOff)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.prepares != 0 {
		t.Fatal("a sink that HAD been working must not trigger the package install")
	}
}

// TestMicBackoffSurvivesTheTUIReopening: closing and reopening the TUI must not
// re-probe every broken or unsupported instance at once; the back-off maps are
// pruned against the running set instead.
func TestMicBackoffSurvivesTheTUIReopening(t *testing.T) {
	h := newMicHarness(t, true)
	h.script = func(int) (*fakeMicSink, bool, error) { return nil, false, nil } // unsupported
	first := openMicStream(t, h.client)
	first.expectDemand(t, false)
	eventually(t, "the instance to be backed off", func() bool {
		h.svc.mic.mu.Lock()
		defer h.svc.mic.mu.Unlock()
		_, ok := h.svc.mic.retryAt["alpha/i1"]
		return ok
	})
	_ = first.stream.CloseSend()
	eventually(t, "the provider to leave", func() bool {
		h.svc.mic.mu.Lock()
		defer h.svc.mic.mu.Unlock()
		return len(h.svc.mic.providers) == 0
	})

	second := openMicStream(t, h.client)
	second.expectDemand(t, false)
	time.Sleep(100 * time.Millisecond)
	h.mu.Lock()
	opens := len(h.opens)
	h.mu.Unlock()
	if opens != 1 {
		t.Fatalf("reopening the TUI re-probed a backed-off instance (%d opens)", opens)
	}

	// …and entries die with their instance / container.
	h.svc.mic.mu.Lock()
	h.svc.mic.preparedAt["c1"] = time.Now()
	h.svc.mic.preparedAt["c-long-gone"] = time.Now()
	h.svc.mic.mu.Unlock()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{}}); err != nil {
		t.Fatal(err)
	}
	h.svc.mic.poke()
	eventually(t, "the back-off maps to be pruned", func() bool {
		h.svc.mic.mu.Lock()
		defer h.svc.mic.mu.Unlock()
		return len(h.svc.mic.retryAt) == 0 && len(h.svc.mic.preparedAt) == 0
	})
}

// TestSetConfigFromAPreMicClientLeavesTheMicAlone: a client built before the
// mic group existed omits it. That must read as "unchanged" — not as "off",
// which would kick every provider and stop every instance's sound server because
// someone changed an unrelated setting from an older TUI.
func TestSetConfigFromAPreMicClientLeavesTheMicAlone(t *testing.T) {
	h := newMicHarness(t, true)
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)
	h.nextSink(t).emit("ready")

	config, err := state.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.MicSettings.Device = "pulse:yeti"
	if err := state.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	old := protoconv.ConfigToProto(config)
	old.Mic = nil // what an old client sends
	old.Dotfiles.AutoInstall = true
	if _, err := h.client.SetConfig(context.Background(), &fleetgrpc.SetConfigRequest{Config: old}); err != nil {
		t.Fatal(err)
	}

	saved, err := state.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !saved.MicSettings.Enabled || saved.MicSettings.Device != "pulse:yeti" {
		t.Fatalf("an absent mic group changed the settings: %+v", saved.MicSettings)
	}
	if !saved.DotfilesSettings.AutoInstall {
		t.Fatal("the setting the old client DID send was lost")
	}
	select {
	case err := <-provider.done:
		t.Fatalf("the provider was kicked: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.stops) != 0 {
		t.Fatalf("sound servers were stopped: %v", h.stops)
	}
}

// The literals the fake sink emits in these tests are the protocol. Pin them to
// the constants both real sides share, so the three cannot drift apart.
func TestMicSinkProtocolLiterals(t *testing.T) {
	for literal, constant := range map[string]string{
		"ready":    micsink.EventReady,
		"demand 1": micsink.EventDemand + " " + micsink.DemandOn,
		"demand 0": micsink.EventDemand + " " + micsink.DemandOff,
		"error":    micsink.EventError,
	} {
		if literal != constant {
			t.Errorf("protocol drift: tests emit %q, micsink defines %q", literal, constant)
		}
	}
}

// A provider may be remote. A frame far beyond the 40 ms contract is refused
// outright rather than parked, 64-deep, in every sink's queue.
func TestMicRejectsOversizedFrames(t *testing.T) {
	h := newMicHarness(t, true)
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)
	sink := h.nextSink(t)
	sink.emit("ready")
	sink.emit("demand 1")
	provider.expectDemand(t, true, "alpha/i1")

	provider.sendAudio(t, make([]byte, maxMicFrameBytes)) // at the bound: fine
	eventually(t, "a frame at the bound to be routed", func() bool { return len(sink.received()) == maxMicFrameBytes })

	provider.sendAudio(t, make([]byte, maxMicFrameBytes+2))
	select {
	case err := <-provider.done:
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("err = %v, want InvalidArgument", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an oversized frame must end the stream")
	}
	if got := len(sink.received()); got != maxMicFrameBytes {
		t.Fatalf("the oversized frame reached the sink (%d bytes)", got)
	}
}

// stubMicSetting replaces the config read for the rest of the test. Call it
// BEFORE newMicHarness: the hub's sync loop reads the seam from its own
// goroutine, so it must be in place before that loop starts (and, cleanups
// being LIFO, it is then restored only after the loop has stopped).
func stubMicSetting(t *testing.T, read func() (bool, error)) {
	t.Helper()
	orig := micSetting
	micSetting = func() (state.MicSettings, error) {
		enabled, err := read()
		return state.MicSettings{Enabled: enabled}, err
	}
	t.Cleanup(func() { micSetting = orig })
}

// EVERY exit from attach must hand the registration back. The one that did not
// — the setting reading as off (or unreadable: a config.json caught mid-write)
// between the sink failing and the install decision — left the key occupied, so
// sync skipped that instance forever: no microphone until the TUI was reopened.
func TestMicAttachReleasesItsSlotWhenTheSettingFlickers(t *testing.T) {
	var mu sync.Mutex
	flicker := false
	stubMicSetting(t, func() (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if flicker {
			flicker = false // one bad read, exactly where attach re-checks
			return false, fmt.Errorf("unexpected end of JSON input")
		}
		return true, nil
	})
	h := newMicHarness(t, true)
	h.script = func(open int) (*fakeMicSink, bool, error) {
		if open == 0 {
			mu.Lock()
			flicker = true
			mu.Unlock()
			return brokenSink(), true, nil
		}
		return newFakeMicSink(), true, nil
	}
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)

	h.nextSink(t)            // fails; the re-check then reads garbage
	healthy := h.nextSink(t) // …and the instance must still get another go
	healthy.emit("ready")
	healthy.emit("demand 1")
	provider.expectDemand(t, true, "alpha/i1")
}

// "The config could not be read" is not "the microphone is off": it must not
// tear the feature down, and must not tell the user to flip a toggle that is on.
func TestMicUnreadableConfigIsNotDisabled(t *testing.T) {
	var unreadable atomic.Bool
	stubMicSetting(t, func() (bool, error) {
		if unreadable.Load() {
			return false, fmt.Errorf("unexpected end of JSON input")
		}
		return true, nil
	})
	h := newMicHarness(t, true)
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)
	sink := h.nextSink(t)
	sink.emit("ready")

	unreadable.Store(true)
	h.svc.mic.poke()
	time.Sleep(150 * time.Millisecond)
	if sink.isClosed() {
		t.Fatal("an unreadable config tore the sinks down")
	}
	select {
	case err := <-provider.done:
		t.Fatalf("an unreadable config kicked the provider: %v", err)
	default:
	}

	late := openMicStream(t, h.client)
	select {
	case err := <-late.done:
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("err = %v, want Unavailable (FailedPrecondition renders as \"enable it in Settings\")", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stream opened while the config is unreadable should be refused")
	}
}

// A config.json switched off BY HAND never goes through SetConfig, so sync is
// what notices — and it must keep the same promise: "off" means no microphone
// in the instances, not a silent one recorders still happily open.
func TestMicHandEditedDisableStopsTheSoundServers(t *testing.T) {
	h := newMicHarness(t, true)
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)
	sink := h.nextSink(t)
	sink.emit("ready")

	if err := state.SaveConfig(&state.Config{MicSettings: state.MicSettings{Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	h.svc.mic.poke()

	eventually(t, "the sink to be closed", sink.isClosed)
	eventually(t, "the instance's sound server to be stopped", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.stops) >= 1
	})
	// Once per edge, not once per tick.
	time.Sleep(2*micSyncInterval + 200*time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.stops) != 1 {
		t.Fatalf("sound servers were stopped %d times; want once for the enabled->disabled edge", len(h.stops))
	}
}

// expectDevice reads demand frames until one carries want as the device.
func (ms *micStream) expectDevice(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case demand := <-ms.demands:
			if demand.GetDevice() == want {
				return
			}
		case err := <-ms.done:
			t.Fatalf("stream ended while waiting for device %q: %v", want, err)
		case <-deadline:
			t.Fatalf("no demand frame carried device %q", want)
		}
	}
}

// The daemon owns the config, so it PUSHES the selected device with the demand:
// in the greeting, and again the moment the selection changes — a provider never
// has to ask, least of all at capture start.
func TestMicDemandCarriesTheSelectedDevice(t *testing.T) {
	h := newMicHarness(t, true)
	config, err := state.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.MicSettings.Device = "pulse:desk_mic"
	if err := state.SaveConfig(config); err != nil {
		t.Fatal(err)
	}

	provider := openMicStream(t, h.client)
	provider.expectDevice(t, "pulse:desk_mic") // the greeting

	config.MicSettings.Device = "pulse:headset"
	if _, err := h.client.SetConfig(context.Background(), &fleetgrpc.SetConfigRequest{Config: protoconv.ConfigToProto(config)}); err != nil {
		t.Fatal(err)
	}
	provider.expectDevice(t, "pulse:headset") // a device-only change republishes

	config.MicSettings.Device = ""
	if _, err := h.client.SetConfig(context.Background(), &fleetgrpc.SetConfigRequest{Config: protoconv.ConfigToProto(config)}); err != nil {
		t.Fatal(err)
	}
	provider.expectDevice(t, "") // back to the system default
}

// A sink that never says anything must not hold its instance's slot for the
// whole provider session: no "ready" in time → closed → retired → retried.
func TestMicSilentSinkTimesOut(t *testing.T) {
	orig := micSinkReadyTimeout
	micSinkReadyTimeout = 150 * time.Millisecond
	t.Cleanup(func() { micSinkReadyTimeout = orig })

	h := newMicHarness(t, true)
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)

	silent := h.nextSink(t) // never emits "ready"
	eventually(t, "the silent sink to be closed", silent.isClosed)
	next := h.nextSink(t) // …and the instance gets another attempt
	next.emit("ready")
	next.emit("demand 1")
	provider.expectDemand(t, true, "alpha/i1")
}

// sync reads the config and the state WITHOUT the lock. A teardown that
// completes in that window must win: acting on the stale read would inject a
// sink (and a detached sound server) into every instance right after "off".
func TestMicSyncDoesNotActOnInputsFromBeforeATeardown(t *testing.T) {
	var inSetting sync.WaitGroup
	release := make(chan struct{})
	var block atomic.Bool
	stubMicSetting(t, func() (bool, error) {
		if block.CompareAndSwap(true, false) {
			inSetting.Done()
			<-release // sync is now parked between its checks and its act
		}
		return true, nil
	})
	h := newMicHarness(t, true)
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)
	h.nextSink(t).emit("ready")

	// Drop the sink so the next sync has something to (re)create, park that sync
	// mid-read, and tear everything down underneath it.
	h.svc.mic.closeAllSinks()
	h.mu.Lock()
	before := len(h.opens)
	h.mu.Unlock()
	inSetting.Add(1)
	block.Store(true)
	h.svc.mic.poke()
	inSetting.Wait()
	h.svc.mic.closeAllSinks() // the teardown that must win
	close(release)

	time.Sleep(200 * time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.opens) != before {
		t.Fatalf("a sync that read its inputs before a teardown still opened %d sink(s)", len(h.opens)-before)
	}
}
