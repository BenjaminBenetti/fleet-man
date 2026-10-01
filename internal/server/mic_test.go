package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
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

// micStream is a provider stream with its demand frames (and device-listing
// requests) pumped onto channels.
type micStream struct {
	stream  fleetgrpc.FleetService_MicClient
	demands chan *fleetgrpc.MicDemand
	relists chan struct{}
	done    chan error
	cancel  context.CancelFunc
}

// openMicStream attaches an anonymous provider — what a client built before
// providers announced a name looks like.
func openMicStream(t *testing.T, client fleetgrpc.FleetServiceClient) *micStream {
	t.Helper()
	return openMicStreamAs(t, client, "")
}

// openMicStreamAs attaches a provider announcing itself as the machine `name`.
func openMicStreamAs(t *testing.T, client fleetgrpc.FleetServiceClient, name string) *micStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := client.Mic(ctx)
	if err != nil {
		t.Fatalf("Mic: %v", err)
	}
	if err := stream.Send(&fleetgrpc.MicUp{Msg: &fleetgrpc.MicUp_Open{Open: &fleetgrpc.MicOpen{
		SampleRate: mic.SampleRate, Channels: mic.Channels, Client: name,
	}}}); err != nil {
		t.Fatalf("send open: %v", err)
	}
	ms := &micStream{
		stream:  stream,
		demands: make(chan *fleetgrpc.MicDemand, 16),
		relists: make(chan struct{}, 16),
		done:    make(chan error, 1),
		cancel:  cancel,
	}
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
			if down.GetListDevices() != nil {
				ms.relists <- struct{}{}
			}
		}
	}()
	return ms
}

// sendDevices announces the provider's capture devices ("id=label" each).
func (ms *micStream) sendDevices(t *testing.T, devices ...string) {
	t.Helper()
	list := &fleetgrpc.MicDeviceList{}
	for _, device := range devices {
		id, label, _ := strings.Cut(device, "=")
		list.Devices = append(list.Devices, &fleetgrpc.MicDevice{Id: id, Label: label})
	}
	if err := ms.stream.Send(&fleetgrpc.MicUp{Msg: &fleetgrpc.MicUp_Devices{Devices: list}}); err != nil {
		t.Fatalf("send devices: %v", err)
	}
}

// nextDemand returns the next demand frame.
func (ms *micStream) nextDemand(t *testing.T) *fleetgrpc.MicDemand {
	t.Helper()
	select {
	case demand := <-ms.demands:
		return demand
	case err := <-ms.done:
		t.Fatalf("stream ended while waiting for demand: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no demand frame")
	}
	return nil
}

// expectNoDemand fails if a demand frame arrives: a stand-by provider must not
// be told anything when the source's demand changes.
func (ms *micStream) expectNoDemand(t *testing.T) {
	t.Helper()
	select {
	case demand := <-ms.demands:
		t.Fatalf("unexpected demand frame: %v", demand)
	case <-time.After(50 * time.Millisecond):
	}
}

// setMicSelection saves the microphone selection the way the settings page
// does: through SetConfig.
func (h *micHarness) setMicSelection(t *testing.T, client, device string) {
	t.Helper()
	config, err := state.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.MicSettings.Client, config.MicSettings.Device = client, device
	if _, err := h.client.SetConfig(context.Background(), &fleetgrpc.SetConfigRequest{Config: protoconv.ConfigToProto(config)}); err != nil {
		t.Fatal(err)
	}
}

// micSourceNames renders the daemon's source list as "name[*][!](ids…)" —
// * marks the client being recorded, ! that its microphone is open.
func (h *micHarness) micSourceNames(t *testing.T) string {
	t.Helper()
	reply, err := h.client.ListMicSources(context.Background(), &fleetgrpc.ListMicSourcesRequest{})
	if err != nil {
		t.Fatalf("ListMicSources: %v", err)
	}
	var out []string
	for _, source := range reply.GetSources().GetSources() {
		entry := source.GetClient()
		if source.GetSource() {
			entry += "*"
		}
		if source.GetRecording() {
			entry += "!"
		}
		if source.GetDevicesListed() {
			var ids []string
			for _, device := range source.GetDevices() {
				ids = append(ids, device.GetId())
			}
			entry += "(" + strings.Join(ids, ",") + ")"
		}
		out = append(out, entry)
	}
	return strings.Join(out, " ")
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

// OPENING a sink can hang as well (it probes docker for the container's user);
// parked there, attach would hold the instance's slot with no retire and no
// back-off, for the daemon's lifetime. It is bounded, and a sink that turns up
// after the deadline is closed rather than leaked.
func TestMicHungSinkOpenTimesOut(t *testing.T) {
	orig := micSinkOpenTimeout
	micSinkOpenTimeout = 150 * time.Millisecond
	t.Cleanup(func() { micSinkOpenTimeout = orig })

	h := newMicHarness(t, true)
	release := make(chan struct{})
	late := newFakeMicSink()
	h.script = func(open int) (*fakeMicSink, bool, error) {
		if open == 0 {
			<-release // a wedged dockerd
			return late, true, nil
		}
		return newFakeMicSink(), true, nil
	}
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)

	eventually(t, "the hung open to be given up on and the instance backed off", func() bool {
		h.svc.mic.mu.Lock()
		defer h.svc.mic.mu.Unlock()
		_, backedOff := h.svc.mic.retryAt["alpha/i1"]
		_, stillHeld := h.svc.mic.sinks["alpha/i1"]
		return backedOff && !stillHeld
	})
	close(release)
	eventually(t, "the late sink to be closed, not leaked", late.isClosed)
}

// An attach ABANDONED while queued for an install slot installed nothing, so it
// must hand back its "recently prepared" stamp — or closing the TUI with
// installs queued leaves those instances mic-less for the whole retry window.
func TestMicAbandonedPrepareHandsBackItsStamp(t *testing.T) {
	h := newMicHarness(t, true)
	h.script = func(int) (*fakeMicSink, bool, error) { return brokenSink(), true, nil }
	// Hold both install slots so the attach queues.
	for range micPrepareParallel {
		h.svc.mic.prepareSlots <- struct{}{}
	}
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)
	h.nextSink(t)
	eventually(t, "the attach to stamp and queue", func() bool {
		h.svc.mic.mu.Lock()
		defer h.svc.mic.mu.Unlock()
		_, stamped := h.svc.mic.preparedAt["c1"]
		return stamped
	})

	_ = provider.stream.CloseSend() // the TUI closes: the queued attach is dropped
	eventually(t, "the provider to leave", func() bool {
		h.svc.mic.mu.Lock()
		defer h.svc.mic.mu.Unlock()
		return len(h.svc.mic.providers) == 0
	})
	for range micPrepareParallel {
		<-h.svc.mic.prepareSlots // let the queued attach through: it must see "dropped"
	}
	eventually(t, "the abandoned attempt to hand back its stamp", func() bool {
		h.svc.mic.mu.Lock()
		defer h.svc.mic.mu.Unlock()
		_, stamped := h.svc.mic.preparedAt["c1"]
		return !stamped
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.prepares != 0 {
		t.Fatalf("an abandoned attach still ran the install (%d)", h.prepares)
	}
}

// The feature: with several clients attached, the one Settings selects is the
// source — NOT whichever attached last. Its audio is the audio routed, and the
// others stand by with their microphones closed.
func TestMicSelectedClientIsTheSource(t *testing.T) {
	h := newMicHarness(t, true)
	h.setMicSelection(t, "desk", "pulse:desk_usb")

	desk := openMicStreamAs(t, h.client, "desk")
	if greeting := desk.nextDemand(t); greeting.GetActive() || greeting.GetDevice() != "pulse:desk_usb" || greeting.GetClient() != "desk" {
		t.Fatalf("desk greeting = %v, want idle with its selected device", greeting)
	}
	sink := h.nextSink(t)
	sink.emit("ready")

	// A newer client attaches. Before clients could be selected it would have
	// taken the microphone over; now it is greeted idle and stands by.
	laptop := openMicStreamAs(t, h.client, "laptop")
	if greeting := laptop.nextDemand(t); greeting.GetActive() || greeting.GetDevice() != "" {
		t.Fatalf("laptop greeting = %v, want idle and no device (it is not the selected client)", greeting)
	}

	sink.emit("demand 1")
	desk.expectDemand(t, true, "alpha/i1")
	laptop.expectNoDemand(t)

	laptop.sendAudio(t, []byte("laptop"))
	desk.sendAudio(t, []byte("desk"))
	eventually(t, "the selected client's audio", func() bool { return string(sink.received()) == "desk" })
}

// Changing the selection moves the microphone at once, mid-recording: the old
// source is told to close, the new one to open — on its own device.
func TestMicSelectionChangeMovesTheMicrophone(t *testing.T) {
	h := newMicHarness(t, true)
	h.setMicSelection(t, "desk", "pulse:desk_usb")
	desk := openMicStreamAs(t, h.client, "desk")
	desk.expectDemand(t, false)
	laptop := openMicStreamAs(t, h.client, "laptop")
	laptop.expectDemand(t, false)
	sink := h.nextSink(t)
	sink.emit("ready")
	sink.emit("demand 1")
	desk.expectDemand(t, true, "alpha/i1")

	h.setMicSelection(t, "laptop", "pulse:laptop_builtin")

	desk.expectDemand(t, false)
	moved := laptop.nextDemand(t)
	if !moved.GetActive() || moved.GetDevice() != "pulse:laptop_builtin" || moved.GetClient() != "laptop" {
		t.Fatalf("laptop was told %v, want live on its own device", moved)
	}
	desk.sendAudio(t, []byte("desk"))
	laptop.sendAudio(t, []byte("laptop"))
	eventually(t, "the newly selected client's audio", func() bool { return string(sink.received()) == "laptop" })

	// Back to no selection: the most recently attached client records.
	h.setMicSelection(t, "", "")
	if demand := laptop.nextDemand(t); !demand.GetActive() || demand.GetDevice() != "" || demand.GetClient() != "" {
		t.Fatalf("laptop was told %v, want live on the default with no selection", demand)
	}
	desk.expectNoDemand(t)
}

// A selected client that is not attached must not mean a dead microphone: the
// most recently attached client stands in — on its DEFAULT device (the selected
// id belongs to the other machine's enumeration) and told whom it stands in for
// — and hands the microphone over the moment the selected client attaches.
func TestMicStandsInForAnAbsentSelectedClient(t *testing.T) {
	h := newMicHarness(t, true)
	h.setMicSelection(t, "desk", "pulse:desk_usb")

	laptop := openMicStreamAs(t, h.client, "laptop")
	if greeting := laptop.nextDemand(t); greeting.GetDevice() != "" || greeting.GetClient() != "desk" {
		t.Fatalf("stand-in greeting = %v, want no device and the selected client named", greeting)
	}
	sink := h.nextSink(t)
	sink.emit("ready")
	sink.emit("demand 1")
	laptop.expectDemand(t, true, "alpha/i1")
	if got := h.micSourceNames(t); got != "laptop*!" {
		t.Fatalf("sources = %q, want laptop recording", got)
	}

	desk := openMicStreamAs(t, h.client, "desk")
	if demand := desk.nextDemand(t); !demand.GetActive() || demand.GetDevice() != "pulse:desk_usb" {
		t.Fatalf("desk was told %v, want live on its selected device", demand)
	}
	laptop.expectDemand(t, false)
	eventually(t, "desk to be the source", func() bool { return h.micSourceNames(t) == "desk*! laptop" })

	desk.cancel() // the selected client goes away mid-recording
	laptop.expectDemand(t, true, "alpha/i1")
	eventually(t, "laptop to stand in again", func() bool { return h.micSourceNames(t) == "laptop*!" })
}

// The source list is what the selector offers: one entry per MACHINE with its
// devices, sorted, the recorded one marked. Two providers on one machine are one
// source; a provider with no name cannot be selected and is not listed.
func TestMicSourcesListEveryClientsDevices(t *testing.T) {
	h := newMicHarness(t, true)
	if got := h.micSourceNames(t); got != "" {
		t.Fatalf("sources with nobody attached = %q", got)
	}

	laptop := openMicStreamAs(t, h.client, "laptop")
	laptop.expectDemand(t, false)
	eventually(t, "laptop to be listed, devices unknown", func() bool { return h.micSourceNames(t) == "laptop*" })
	laptop.sendDevices(t, "pulse:builtin=Built-in Microphone")

	desk := openMicStreamAs(t, h.client, "desk")
	desk.expectDemand(t, false)
	desk.sendDevices(t, "pulse:usb=USB Mic", "pulse:webcam=Webcam")
	eventually(t, "both clients with their devices", func() bool {
		return h.micSourceNames(t) == "desk*(pulse:usb,pulse:webcam) laptop(pulse:builtin)"
	})

	// A second provider on the desk machine (a second TUI) is the same source,
	// described by the newer of the two.
	desk2 := openMicStreamAs(t, h.client, "desk")
	desk2.expectDemand(t, false)
	desk2.sendDevices(t, "pulse:usb=USB Mic")
	eventually(t, "the newer desk provider's listing", func() bool {
		return h.micSourceNames(t) == "desk*(pulse:usb) laptop(pulse:builtin)"
	})

	// An anonymous provider takes the microphone (it is the newest, and nothing
	// is selected) but offers nothing to select.
	old := openMicStream(t, h.client)
	old.expectDemand(t, false)
	eventually(t, "no listed client to be the source", func() bool {
		return h.micSourceNames(t) == "desk(pulse:usb) laptop(pulse:builtin)"
	})

	// An empty listing is an answer too (a default-only recorder).
	laptop.sendDevices(t)
	eventually(t, "laptop's empty listing", func() bool {
		return h.micSourceNames(t) == "desk(pulse:usb) laptop()"
	})
}

// What a provider announces is stored by the daemon and drawn in every other
// client's terminal, and a provider can be anywhere: names and labels are
// scrubbed of control characters, ids that are not printable are dropped, and
// the listing is bounded.
func TestMicSourcesAreScrubbedAndBounded(t *testing.T) {
	h := newMicHarness(t, true)
	provider := openMicStreamAs(t, h.client, "  evil\x1b[31m\nhost\u202e  ")
	provider.expectDemand(t, false)

	devices := []string{"pulse:ok=Good\x1b]0;pwned\x07 Mic", "pulse:bad\x1bid=Escaped Id", "=No Id", "pulse:ok=Duplicate"}
	for i := range 2 * mic.MaxDevices {
		devices = append(devices, fmt.Sprintf("pulse:extra%d=%s", i, strings.Repeat("x", 4*mic.MaxDeviceLabel)))
	}
	provider.sendDevices(t, devices...)

	var source *fleetgrpc.MicSource
	eventually(t, "the listing to arrive", func() bool {
		reply, err := h.client.ListMicSources(context.Background(), &fleetgrpc.ListMicSourcesRequest{})
		if err != nil || len(reply.GetSources().GetSources()) != 1 {
			return false
		}
		source = reply.GetSources().GetSources()[0]
		return source.GetDevicesListed()
	})
	if source.GetClient() != "evil[31m host" {
		t.Fatalf("client = %q, want the control characters gone", source.GetClient())
	}
	if len(source.GetDevices()) != mic.MaxDevices {
		t.Fatalf("%d devices listed, want the bound of %d", len(source.GetDevices()), mic.MaxDevices)
	}
	if first := source.GetDevices()[0]; first.GetId() != "pulse:ok" || first.GetLabel() != "Good]0;pwned Mic" {
		t.Fatalf("first device = %v, want a scrubbed label", first)
	}
	for _, device := range source.GetDevices() {
		if strings.ContainsAny(device.GetId()+device.GetLabel(), "\x1b\x07\n") {
			t.Fatalf("device %v still carries control characters", device)
		}
		if device.GetId() == "pulse:bad\x1bid" || device.GetId() == "" {
			t.Fatalf("device %v should have been dropped", device)
		}
		if len([]rune(device.GetLabel())) > mic.MaxDeviceLabel {
			t.Fatalf("label of %d runes exceeds the bound", len([]rune(device.GetLabel())))
		}
	}
}

// Opening a selector asks every attached provider — not just the source — to
// list its devices again.
func TestMicRelistAsksEveryProvider(t *testing.T) {
	h := newMicHarness(t, true)
	desk := openMicStreamAs(t, h.client, "desk")
	desk.expectDemand(t, false)
	laptop := openMicStreamAs(t, h.client, "laptop")
	laptop.expectDemand(t, false)

	if _, err := h.client.ListMicSources(context.Background(), &fleetgrpc.ListMicSourcesRequest{Refresh: true}); err != nil {
		t.Fatalf("ListMicSources: %v", err)
	}
	for name, provider := range map[string]*micStream{"desk": desk, "laptop": laptop} {
		select {
		case <-provider.relists:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was not asked to list its devices", name)
		}
	}

	// Without refresh nobody is bothered.
	if _, err := h.client.ListMicSources(context.Background(), &fleetgrpc.ListMicSourcesRequest{}); err != nil {
		t.Fatalf("ListMicSources: %v", err)
	}
	select {
	case <-desk.relists:
		t.Fatal("a plain listing must not make providers enumerate")
	case <-time.After(50 * time.Millisecond):
	}
}

// The source list is PUSHED: a TUI with its settings page open sees another
// client connect, list its devices and leave without asking.
func TestWatchPushesMicSources(t *testing.T) {
	h := newMicHarness(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch, err := h.client.Watch(ctx, &fleetgrpc.WatchRequest{IncludeInitialState: true})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	pushed := make(chan *fleetgrpc.MicSources, 32)
	go func() {
		for {
			event, err := watch.Recv()
			if err != nil {
				return
			}
			if sources := event.GetMicSources(); sources != nil {
				pushed <- sources
			}
		}
	}()
	// expect reads pushes until one matches (earlier ones are intermediate
	// states: attached, then listed).
	expect := func(what string, match func(*fleetgrpc.MicSources) bool) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case sources := <-pushed:
				if match(sources) {
					return
				}
			case <-deadline:
				t.Fatalf("no push with %s", what)
			}
		}
	}

	expect("the initial, empty set", func(s *fleetgrpc.MicSources) bool { return len(s.GetSources()) == 0 })

	desk := openMicStreamAs(t, h.client, "desk")
	desk.expectDemand(t, false)
	desk.sendDevices(t, "pulse:usb=USB Mic")
	expect("desk and its device", func(s *fleetgrpc.MicSources) bool {
		return len(s.GetSources()) == 1 && s.GetSources()[0].GetClient() == "desk" &&
			s.GetSources()[0].GetSource() && len(s.GetSources()[0].GetDevices()) == 1
	})

	sink := h.nextSink(t)
	sink.emit("ready")
	sink.emit("demand 1")
	expect("desk recording", func(s *fleetgrpc.MicSources) bool {
		return len(s.GetSources()) == 1 && s.GetSources()[0].GetRecording()
	})

	desk.cancel()
	expect("nobody attached", func(s *fleetgrpc.MicSources) bool { return len(s.GetSources()) == 0 })
}

// A client built before the selection had a client sends the mic group without
// one. That must read as "unchanged": saving an unrelated setting from an older
// TUI must not hand the microphone to a different machine.
func TestSetConfigFromAPreClientSelectionClientKeepsTheClient(t *testing.T) {
	h := newMicHarness(t, true)
	h.setMicSelection(t, "desk", "pulse:desk_usb")

	config, err := state.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	old := protoconv.ConfigToProto(config)
	old.Mic.Client = nil // what an older client's message decodes to
	old.Mic.Device = "pulse:other"
	reply, err := h.client.SetConfig(context.Background(), &fleetgrpc.SetConfigRequest{Config: old})
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if got := reply.GetConfig().GetMic(); got.GetClient() != "desk" || got.GetDevice() != "pulse:other" {
		t.Fatalf("mic after an older client's save = %v, want the client kept and the device applied", got)
	}

	// A current client clearing the selection says so explicitly, and is obeyed.
	h.setMicSelection(t, "", "")
	saved, err := state.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.MicSettings.Client != "" {
		t.Fatalf("client = %q, want the selection cleared", saved.MicSettings.Client)
	}
}

// The QA-found hole: the selection is a daemon setting any client can change,
// but a client fetches the config once. So the daemon pushes the microphone
// settings with the client list — on EVERY config save, with or without a
// provider attached — and every open TUI keeps the selection the daemon holds
// (rather than showing a stale one and writing it back with its next save).
func TestWatchPushesTheMicSettingsOnEverySave(t *testing.T) {
	h := newMicHarness(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch, err := h.client.Watch(ctx, &fleetgrpc.WatchRequest{IncludeInitialState: true})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	pushed := make(chan *fleetgrpc.MicSettings, 32)
	go func() {
		for {
			event, err := watch.Recv()
			if err != nil {
				return
			}
			if sources := event.GetMicSources(); sources != nil && sources.GetSettings() != nil {
				pushed <- sources.GetSettings()
			}
		}
	}()
	expect := func(what string, match func(*fleetgrpc.MicSettings) bool) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case settings := <-pushed:
				if match(settings) {
					return
				}
			case <-deadline:
				t.Fatalf("no push with %s", what)
			}
		}
	}

	// Nothing has happened yet, and nobody is attached: a subscriber is still
	// told what the daemon holds.
	expect("the settings on disk", func(s *fleetgrpc.MicSettings) bool {
		return s.GetEnabled() && s.Client != nil && s.GetClient() == ""
	})

	// Another client makes a selection — no provider anywhere.
	h.setMicSelection(t, "desk", "pulse:desk_usb")
	expect("the new selection", func(s *fleetgrpc.MicSettings) bool {
		return s.GetEnabled() && s.GetClient() == "desk" && s.GetDevice() == "pulse:desk_usb"
	})

	// ListMicSources answers the same thing.
	reply, err := h.client.ListMicSources(context.Background(), &fleetgrpc.ListMicSourcesRequest{})
	if err != nil {
		t.Fatalf("ListMicSources: %v", err)
	}
	if got := reply.GetSources().GetSettings(); got.GetClient() != "desk" || got.GetDevice() != "pulse:desk_usb" {
		t.Fatalf("ListMicSources settings = %v", got)
	}

	// Turning the microphone off is pushed too: a client still holding
	// "enabled" would otherwise turn it back on with its next save.
	config, err := state.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.MicSettings.Enabled = false
	if _, err := h.client.SetConfig(context.Background(), &fleetgrpc.SetConfigRequest{Config: protoconv.ConfigToProto(config)}); err != nil {
		t.Fatal(err)
	}
	expect("the microphone turned off, selection kept", func(s *fleetgrpc.MicSettings) bool {
		return !s.GetEnabled() && s.GetClient() == "desk"
	})
	// Turning it off stops the instances' sound servers on a goroutine of its
	// own; let it finish before the harness restores the seam it calls.
	eventually(t, "the instance's sound server to be stopped", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.stops) == 1
	})
}

// A selection made by editing config.json reaches the clients as well (the hub
// reads it on its sync tick while a provider is attached).
func TestHandEditedMicSelectionIsPushed(t *testing.T) {
	h := newMicHarness(t, true)
	provider := openMicStreamAs(t, h.client, "laptop")
	provider.expectDemand(t, false)

	config, err := state.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	config.MicSettings.Client, config.MicSettings.Device = "laptop", "pulse:builtin"
	if err := state.SaveConfig(config); err != nil { // not through SetConfig
		t.Fatal(err)
	}
	provider.expectDevice(t, "pulse:builtin")
	eventually(t, "the hand-edited selection to be announced", func() bool {
		var cached *fleetgrpc.MicSettings
		done := make(chan struct{})
		if !h.svc.hub.post(func(hub *hub) { cached = hub.micSources.GetSettings(); close(done) }) {
			return false
		}
		<-done
		return cached.GetClient() == "laptop" && cached.GetDevice() == "pulse:builtin"
	})
}

// An unreadable config must not blank the settings a new subscriber is greeted
// with: "absent" tells clients to keep what they have, but the cache should
// still hold the last value the daemon could read.
func TestMicSourcesKeepLastKnownSettingsWhenTheConfigIsUnreadable(t *testing.T) {
	svc := newService()
	orig := micSetting
	t.Cleanup(func() { micSetting = orig })

	micSetting = func() (state.MicSettings, error) {
		return state.MicSettings{Enabled: true, Client: "desk"}, nil
	}
	known := svc.micSourcesNow(nil).GetSettings()
	if known.GetClient() != "desk" || !known.GetEnabled() {
		t.Fatalf("settings = %v", known)
	}

	micSetting = func() (state.MicSettings, error) { return state.MicSettings{}, fmt.Errorf("mid-write") }
	if got := svc.micSourcesNow(known).GetSettings(); got.GetClient() != "desk" {
		t.Fatalf("an unreadable config replaced the last known settings with %v", got)
	}
	if got := svc.micSourcesNow(nil).GetSettings(); got != nil {
		t.Fatalf("with nothing known the settings must be absent, got %v", got)
	}
}
