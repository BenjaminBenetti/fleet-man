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
