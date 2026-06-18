package state

// AgentTool identifies which AI agent CLI to invoke for agent-driven flows.
type AgentTool string

const (
	AgentToolCodex   AgentTool = "codex"
	AgentToolClaude  AgentTool = "claude"
	AgentToolGemini  AgentTool = "gemini"
	AgentToolCopilot AgentTool = "copilot"
	AgentToolAuggie  AgentTool = "auggie"
)

// validAgentTools enumerates the set of recognised agent tools so unknown
// values fall back to the default during config load.
var validAgentTools = map[AgentTool]struct{}{
	AgentToolCodex:   {},
	AgentToolClaude:  {},
	AgentToolGemini:  {},
	AgentToolCopilot: {},
	AgentToolAuggie:  {},
}
