package agentdetect

import (
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// NewDetector returns the appropriate Detector strategy for a given
// agent tool.
//
// Today every tool falls back to TmuxPaneChangeDetector — the generic
// pane-diffing strategy works for any CLI agent that draws into a
// tmux pane. As tool-specific signals are added (e.g., parsing a
// known agent's status line, watching its log files, hitting an
// exposed status endpoint) new branches can be added here without
// touching the call sites.
func NewDetector(tool state.AgentTool) Detector {
	switch tool {
	default:
		return NewTmuxPaneChangeDetector()
	}
}
