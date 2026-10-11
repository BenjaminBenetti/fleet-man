package server

import (
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// recordedFlow is a backend.AudioFlow that records what it was told.
type recordedFlow struct {
	mu      sync.Mutex
	demand  []bool
	audible int
	late    int
}

func (f *recordedFlow) MicDemand(recording bool) {
	f.mu.Lock()
	f.demand = append(f.demand, recording)
	f.mu.Unlock()
}

func (f *recordedFlow) OutputAudible() {
	f.mu.Lock()
	f.audible++
	f.mu.Unlock()
}

func (f *recordedFlow) Late() {
	f.mu.Lock()
	f.late++
	f.mu.Unlock()
}

func (f *recordedFlow) snapshot() (demand []bool, audible, late int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.demand...), f.audible, f.late
}

// withAudioFlow makes every instance's audio flow f for the test.
func withAudioFlow(t *testing.T, f backend.AudioFlow) {
	t.Helper()
	orig := audioFlowFor
	audioFlowFor = func(*fleet.Instance) backend.AudioFlow { return f }
	t.Cleanup(func() { audioFlowFor = orig })
}

// A sink that has said "alive" and then goes quiet is on a dead path (a
// cluster session that stopped answering): it is closed after the deadline,
// and its demand (so the human's microphone) closes with it.
func TestMicSinkThatMissesItsHeartbeatIsClosed(t *testing.T) {
	orig := micAliveDeadline
	micAliveDeadline = 150 * time.Millisecond
	t.Cleanup(func() { micAliveDeadline = orig })

	h := newMicHarness(t, true)
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)
	sink := h.nextSink(t)
	sink.emit("ready")
	sink.emit("alive")
	sink.emit("demand 1")
	provider.expectDemand(t, true, "alpha/i1")

	// Heartbeats keep it open past the deadline.
	for range 4 {
		time.Sleep(micAliveDeadline / 2)
		sink.emit("alive")
	}
	if sink.isClosed() {
		t.Fatal("a sink that kept saying alive was closed")
	}

	start := time.Now()
	provider.expectDemand(t, false) // then silence: the deadline closes it
	if !sink.isClosed() {
		t.Fatal("the silent sink's demand ended but it was not closed")
	}
	if waited := time.Since(start); waited > micAliveDeadline+2*time.Second {
		t.Fatalf("the demand closed %v after the last heartbeat", waited)
	}
	h.nextSink(t) // the instance gets a fresh sink
}

// A sink that never says "alive" (one staged by an older fleet) is not held
// to the deadline: it stays open through a quiet spell.
func TestMicSinkWithoutAHeartbeatIsNotHeldToTheDeadline(t *testing.T) {
	orig := micAliveDeadline
	micAliveDeadline = 100 * time.Millisecond
	t.Cleanup(func() { micAliveDeadline = orig })

	h := newMicHarness(t, true)
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)
	sink := h.nextSink(t)
	sink.emit("ready")
	sink.emit("demand 1")
	provider.expectDemand(t, true, "alpha/i1")
	time.Sleep(5 * micAliveDeadline)
	if sink.isClosed() {
		t.Fatal("a sink without heartbeats was closed by the heartbeat deadline")
	}
	provider.expectNoDemand(t)
}

// The sink's demand and late frames reach the instance's audio flow (what
// paces a cluster pod's bulk transfers), and its end ends the demand.
func TestMicFeedsTheInstancesAudioFlow(t *testing.T) {
	flow := &recordedFlow{}
	withAudioFlow(t, flow)
	h := newMicHarness(t, true)
	provider := openMicStream(t, h.client)
	provider.expectDemand(t, false)
	sink := h.nextSink(t)
	sink.emit("ready")
	sink.emit("demand 1")
	provider.expectDemand(t, true, "alpha/i1")
	sink.emit("late")
	sink.emit("late")
	sink.emit("demand 0")
	provider.expectDemand(t, false)
	eventually(t, "the flow to hear demand and lateness", func() bool {
		demand, _, late := flow.snapshot()
		return len(demand) == 2 && late == 2
	})
	_ = sink.Close()
	eventually(t, "the sink's end to end its demand", func() bool {
		demand, _, _ := flow.snapshot()
		return len(demand) == 3
	})
	demand, _, _ := flow.snapshot()
	if !demand[0] || demand[1] || demand[2] {
		t.Fatalf("flow demand %v, want [true false false]", demand)
	}
}
