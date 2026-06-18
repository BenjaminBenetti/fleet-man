package agentdetect

import (
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// NewDetector returns the appropriate Detector strategy for a given
// agent tool.
//
// Per-tool branches plug in tool-specific signals when they are
// available (e.g., Claude Code's lifecycle hooks). Tools without a
// dedicated strategy fall back to TmuxPaneChangeDetector — the
// generic pane-diffing strategy that works for any CLI agent
// drawing into a tmux pane.
func NewDetector(tool state.AgentTool) Detector {
	switch tool {
	case state.AgentToolClaude:
		return NewClaudeHookDetector()
	case state.AgentToolAuggie:
		return NewAuggieHookDetector()
	default:
		return NewTmuxPaneChangeDetector()
	}
}
