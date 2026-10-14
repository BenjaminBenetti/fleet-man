package server

import (
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestAgentTrackerForgetsPaneHistoryAcrossAGap: a frame-diff agent (codex
// here) that sat idle through a polling gap on a screen that redrew meanwhile
// must not read as working when polling resumes — which the fleet status mod
// would then report as a fresh stop once it settles to waiting.
func TestAgentTrackerForgetsPaneHistoryAcrossAGap(t *testing.T) {
	screen := func(content string) map[string]backend.AllSessions {
		return map[string]backend.AllSessions{"c": {OK: true, Sessions: map[string]backend.ScreenCapture{
			"codex": {OK: true, Content: content},
		}}}
	}
	probes := map[string]string{"c": string(state.AgentToolCodex)}
	afterGap := func(forget bool) agentdetect.State {
		tr := newAgentTracker()
		t0 := time.Unix(1_000, 0)
		tr.Update(screen("› idle, before the gap"), probes, []string{"c"}, t0)
		tr.Update(screen("› idle, before the gap"), probes, []string{"c"}, t0.Add(3*time.Second))
		if forget {
			tr.forgetHistory()
		}
		// Ten minutes unpolled; the idle screen redrew meanwhile.
		tr.Update(screen("› idle, after the gap!!"), probes, []string{"c"}, t0.Add(10*time.Minute))
		return tr.State("c")
	}

	if got := afterGap(false); got != agentdetect.StateWorking {
		t.Fatalf("without forgetting, the diff across the gap reads %v; this test no longer shows the hazard", got)
	}
	if got := afterGap(true); got != agentdetect.StateWaiting {
		t.Fatalf("after forgetting, the first capture past the gap reads %v, want waiting (a fresh seed)", got)
	}
}

// TestAgentTrackerForgetsStatesAcrossAGap: a capture that fails right after
// polling resumes must not carry a pre-gap state forward — an agent working
// when the TUI left and idle since would read as working, then "just stopped"
// on the next good capture. Any agent: the stale value is the tracker's.
func TestAgentTrackerForgetsStatesAcrossAGap(t *testing.T) {
	for _, tool := range []state.AgentTool{state.AgentToolCodex, state.AgentToolClaude} {
		tr := newAgentTracker()
		tr.states["c"] = agentdetect.StateWorking
		tr.tools["c"] = tool
		tr.forgetHistory()
		failed := map[string]backend.AllSessions{"c": {OK: false}}
		tr.Update(failed, nil, []string{"c"}, time.Unix(2_000, 0))
		if got := tr.State("c"); got == agentdetect.StateWorking {
			t.Fatalf("%s: a failed capture after the gap kept the pre-gap Working", tool)
		}
		if got := tr.Tool("c"); got != tool {
			t.Fatalf("%s: the tool was forgotten too (%q)", tool, got)
		}
	}
}

// TestRuntimeEdgeForgetsAgentHistory: the hub forgets the detectors' history
// when activity polling resumes (the runtime gate's false→true edge).
func TestRuntimeEdgeForgetsAgentHistory(t *testing.T) {
	h := newHub()
	h.agent.detectors["c"] = agentdetect.NewDetector(state.AgentToolCodex)
	h.addSub(newSubscriber(false))
	if len(h.agent.detectors) != 1 {
		t.Fatal("a subscriber that does not poll the runtime must not reset the detectors")
	}
	h.addSub(newSubscriber(true))
	if len(h.agent.detectors) != 0 {
		t.Fatalf("detectors kept across the polling edge: %v", h.agent.detectors)
	}
}

// TestAgentTrackerReportsUnknownAfterFailedCaptures: failed captures keep the
// last state for a while (no flicker on a transient failure), until
// unknownAfter of them in a row report StateUnknown; the next good capture
// seeds a fresh detector, and the count starts over.
func TestAgentTrackerReportsUnknownAfterFailedCaptures(t *testing.T) {
	good := map[string]backend.AllSessions{"c": {OK: true, Sessions: map[string]backend.ScreenCapture{
		"codex": {OK: true, Content: "› working"},
	}}}
	failed := map[string]backend.AllSessions{"c": {OK: false}}
	probes := map[string]string{"c": string(state.AgentToolCodex)}
	tr := newAgentTracker()
	t0 := time.Unix(3_000, 0)
	tr.Update(good, probes, []string{"c"}, t0)
	tr.states["c"] = agentdetect.StateWorking // as a later diff would find it
	for i := 1; i < unknownAfter; i++ {
		tr.Update(failed, nil, []string{"c"}, t0.Add(time.Duration(i)*3*time.Second))
		if got := tr.State("c"); got != agentdetect.StateWorking {
			t.Fatalf("after %d failed captures the state is %v, want the last one (working)", i, got)
		}
	}
	tr.Update(failed, nil, []string{"c"}, t0.Add(15*time.Second))
	if got := tr.State("c"); got != agentdetect.StateUnknown {
		t.Fatalf("after %d failed captures the state is %v, want unknown", unknownAfter, got)
	}
	if _, kept := tr.detectors["c"]; kept {
		t.Fatal("an unknown container kept its detector; the next good capture must seed a fresh one")
	}
	if got := tr.Tool("c"); got != state.AgentToolCodex {
		t.Fatalf("the tool was forgotten (%q)", got)
	}
	tr.Update(failed, nil, []string{"c"}, t0.Add(18*time.Second))
	if got := tr.State("c"); got != agentdetect.StateUnknown {
		t.Fatalf("a further failed capture reads %v, want still unknown", got)
	}

	tr.Update(good, probes, []string{"c"}, t0.Add(21*time.Second))
	if got := tr.State("c"); got != agentdetect.StateWaiting {
		t.Fatalf("the good capture after unknown reads %v, want waiting (a fresh seed)", got)
	}
	tr.Update(failed, nil, []string{"c"}, t0.Add(24*time.Second))
	if got := tr.State("c"); got != agentdetect.StateWaiting {
		t.Fatalf("one failed capture after recovering reads %v; the count did not start over", got)
	}
}

// TestAgentTrackerForgetsFailuresAcrossAGap: failed captures before a polling
// gap do not count toward unknown after it.
func TestAgentTrackerForgetsFailuresAcrossAGap(t *testing.T) {
	failed := map[string]backend.AllSessions{"c": {OK: false}}
	tr := newAgentTracker()
	for i := 0; i < unknownAfter-1; i++ {
		tr.Update(failed, nil, []string{"c"}, time.Unix(int64(4_000+i), 0))
	}
	tr.forgetHistory()
	tr.Update(failed, nil, []string{"c"}, time.Unix(5_000, 0))
	if got := tr.State("c"); got == agentdetect.StateUnknown {
		t.Fatal("failed captures from before the gap made the first one after it unknown")
	}
}
