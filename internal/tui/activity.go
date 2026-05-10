package tui

import (
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentdetect"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// ===========================================
// ActivityTracker
// ===========================================

// ActivityTracker is the generic, strategy-agnostic owner of agent
// run-state across all tracked containers.
//
// It is responsible for:
//
//   - Tool detection (from process probes).
//   - Lifecycle of per-container Detector strategies — built via
//     agentdetect.NewDetector when a container's tool first appears
//     or changes, discarded when the agent disappears.
//   - The transient-failure semantics that apply regardless of
//     strategy: an OK=false capture preserves the previous state so
//     the UI does not flicker through "not running" when an exec
//     blips.
//
// The actual working/waiting decision is delegated to whichever
// agentdetect.Detector the factory returned for the container.
type ActivityTracker struct {
	// ===========================================
	// Fields
	// ===========================================

	states    map[string]agentdetect.State     // containerID → last derived state
	tools     map[string]state.AgentTool       // containerID → last detected agent tool
	detectors map[string]agentdetect.Detector  // containerID → per-container strategy
}

// ===========================================
// Constructors
// ===========================================

// NewActivityTracker returns an initialised tracker with no tracked
// containers.
func NewActivityTracker() *ActivityTracker {
	return &ActivityTracker{
		states:    make(map[string]agentdetect.State),
		tools:     make(map[string]state.AgentTool),
		detectors: make(map[string]agentdetect.Detector),
	}
}

// ===========================================
// Public Methods
// ===========================================

// State returns the derived agent state for a container. Unknown
// containers return agentdetect.StateNotRunning (the zero value).
func (t *ActivityTracker) State(containerID string) agentdetect.State {
	return t.states[containerID]
}

// Tool returns the detected agent tool for a container, or the empty
// string when no tool has been observed.
func (t *ActivityTracker) Tool(containerID string) state.AgentTool {
	return t.tools[containerID]
}

// Update processes new captures and probe results to derive agent
// states and tool identifications.
//
// Tool detection uses process-based probes (ps aux inside containers)
// and is independent of screen capture success — a probe finding
// claude is recorded even if every tmux capture failed transiently.
//
// State detection per container:
//   - Container missing from captures, or OK=false → preserve
//     previous state, tool, and detector (transient exec failure).
//   - Probe explicitly returned no tool → StateNotRunning, detector
//     dropped so a future agent starts from a clean slate.
//   - Otherwise the per-container Detector (built/refreshed via the
//     factory based on the current tool) decides the state.
//
// Containers absent from expectedIDs are dropped (cleanup).
func (t *ActivityTracker) Update(
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
			// Capture exec failed — preserve previous state to avoid flicker.
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
			// Detector intentionally dropped: when a new agent starts
			// later we want a fresh strategy keyed off the new tool.
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

// ===========================================
// Private Methods
// ===========================================

// applyProbe records the tool for a container based on the probe
// result, falling back to the previously-known tool when no probe
// ran this cycle. Returns true when the probe explicitly confirmed
// that no agent is running.
func (t *ActivityTracker) applyProbe(
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

// detectorFor returns the Detector for a container, building a fresh
// one when none exists yet or when the detected tool has changed
// since the last cycle.
func (t *ActivityTracker) detectorFor(containerID string, tool state.AgentTool) agentdetect.Detector {
	if existing, ok := t.detectors[containerID]; ok && t.tools[containerID] == tool {
		return existing
	}
	return agentdetect.NewDetector(tool)
}
