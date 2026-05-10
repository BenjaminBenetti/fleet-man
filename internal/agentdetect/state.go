// Package agentdetect determines whether the AI agent inside a
// container is currently working, waiting for input, or absent.
//
// Detection is split behind a strategy interface so different agent
// tools (claude, codex, gemini, copilot) can plug in tool-specific
// signals when they are available, while the generic tmux pane-change
// strategy remains the safe default fallback.
package agentdetect

// State represents the runtime status of an agent inside a container.
type State int

const (
	// StateNotRunning means no agent process was detected, or the
	// expected session/process artifact is absent.
	StateNotRunning State = iota
	// StateWorking means the agent is actively producing output.
	StateWorking
	// StateWaiting means the agent is alive but idle (e.g., waiting
	// for user input or a tool result).
	StateWaiting
)
