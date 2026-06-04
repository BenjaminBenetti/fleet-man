package server

import (
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// agentTracker is the server-side relocation of the TUI's ActivityTracker: the
// strategy-agnostic owner of agent run-state across tracked containers. It is
// mutated only on the hub loop (single-threaded), so it needs no internal
// locking. The stateful per-container Detectors (frame-diff history, claude-hook
// reads) live here for the server's lifetime; on a version-restart the history
// resets and seed-on-first-capture (in the detectors) avoids a misleading flip.
//
// This is a near-verbatim port of internal/tui/activity.go (which the TUI keeps
// until Step 7 deletes it, once the server owns the live read path).
type agentTracker struct {
	states    map[string]agentdetect.State
	tools     map[string]state.AgentTool
	detectors map[string]agentdetect.Detector
}

func newAgentTracker() *agentTracker {
	return &agentTracker{
		states:    make(map[string]agentdetect.State),
		tools:     make(map[string]state.AgentTool),
		detectors: make(map[string]agentdetect.Detector),
	}
}

// State returns the derived agent state for a container (zero value
// StateNotRunning for unknown containers).
func (t *agentTracker) State(containerID string) agentdetect.State {
	return t.states[containerID]
}

// Tool returns the detected agent tool for a container, or "" when none has been
// observed.
func (t *agentTracker) Tool(containerID string) state.AgentTool {
	return t.tools[containerID]
}

// Update processes new captures and probe results to derive agent states and
// tool identifications. An OK=false capture preserves the previous state (no
// flicker on a transient exec failure); an explicit empty probe drops the agent
// to NotRunning and discards the detector. Containers absent from expectedIDs
// are dropped.
func (t *agentTracker) Update(
	captures map[string]backend.AllSessions,
	probes map[string]string,
	expectedIDs []string,
	now time.Time,
) {
	newStates := make(map[string]agentdetect.State, len(expectedIDs))
	newTools := make(map[string]state.AgentTool, len(expectedIDs))
	newDetectors := make(map[string]agentdetect.Detector, len(expectedIDs))

	for _, id := range expectedIDs {
		capture, captured := captures[id]
		if !captured || !capture.OK {
			if prev, ok := t.states[id]; ok {
				newStates[id] = prev
			}
			if tool, ok := t.tools[id]; ok {
				newTools[id] = tool
			}
			if existing, ok := t.detectors[id]; ok {
				newDetectors[id] = existing
			}
			continue
		}

		probeConfirmedEmpty := t.applyProbe(id, probes, newTools)
		if probeConfirmedEmpty {
			newStates[id] = agentdetect.StateNotRunning
			continue
		}

		detector := t.detectorFor(id, newTools[id])
		newDetectors[id] = detector
		newStates[id] = detector.Detect(capture, now)
	}

	t.states = newStates
	t.tools = newTools
	t.detectors = newDetectors
}

func (t *agentTracker) applyProbe(
	containerID string,
	probes map[string]string,
	newTools map[string]state.AgentTool,
) bool {
	if probes == nil {
		if tool, ok := t.tools[containerID]; ok {
			newTools[containerID] = tool
		}
		return false
	}
	probeTool, probed := probes[containerID]
	if !probed {
		if tool, ok := t.tools[containerID]; ok {
			newTools[containerID] = tool
		}
		return false
	}
	if probeTool == "" {
		return true
	}
	newTools[containerID] = state.AgentTool(probeTool)
	return false
}

func (t *agentTracker) detectorFor(containerID string, tool state.AgentTool) agentdetect.Detector {
	if existing, ok := t.detectors[containerID]; ok && t.tools[containerID] == tool {
		return existing
	}
	return agentdetect.NewDetector(tool)
}
