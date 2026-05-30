package agent

import (
	"fmt"
	"os/exec"
)

// agentEntry describes a coding agent CLI binary and how to invoke it.
type agentEntry struct {
	Name   string
	Binary string
	Args   func(prompt string) []string
}

// agents lists coding agents in priority order.
var agents = []agentEntry{
	{
		Name:   "Claude Code",
		Binary: "claude",
		Args:   func(prompt string) []string { return []string{prompt} },
	},
	{
		Name:   "Codex",
		Binary: "codex",
		Args:   func(prompt string) []string { return []string{prompt} },
	},
	{
		Name:   "Gemini",
		Binary: "gemini",
		Args:   func(prompt string) []string { return []string{prompt} },
	},
	{
		Name:   "Copilot",
		Binary: "copilot",
		Args:   func(prompt string) []string { return []string{prompt} },
	},
}

// findAgentEntry returns the first available agent entry.
func findAgentEntry() (*agentEntry, error) {
	for i := range agents {
		if _, err := exec.LookPath(agents[i].Binary); err == nil {
			return &agents[i], nil
		}
	}
	return nil, fmt.Errorf("no coding agent found on PATH (tried: claude, codex, gemini, copilot)")
}
