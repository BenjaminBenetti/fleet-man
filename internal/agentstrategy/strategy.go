// Package agentstrategy is how fleet integrates with each kind of coding agent
// it launches — Claude Code, Codex, Auggie, ... — as one Strategy per agent
// tool, picked from an automation agent's launch command (ForCommand).
//
// A strategy knows how to hand ONE launch of its agent the fleet MCP server
// and the Fleet Admiral skill (issue #219). It must never install them where
// every agent would see them: a fleet's agent config (~/.claude, ~/.claude.json,
// ...) is shared by all of its instances, and most agents are not meant to
// drive fleet. So a strategy writes what it needs into a directory owned by the
// launch and points the agent at it through the launch's own environment. Only
// Claude Code implements it so far; the others report ok=false.
package agentstrategy

import (
	"os"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// Strategy integrates fleet with one kind of coding agent.
type Strategy interface {
	// Tool names the agent this strategy is for ("" when unknown).
	Tool() state.AgentTool
	// SupportsFleetMCP reports whether this agent can be handed the fleet MCP.
	SupportsFleetMCP() bool
	// FleetMCP returns what gives one launch of the agent the fleet MCP
	// server and the Fleet Admiral skill, or ok=false when this agent has no
	// way to take them yet (or p is incomplete).
	FleetMCP(p FleetMCPParams) (setup FleetMCPSetup, ok bool)
}

// FleetMCPParams describes one launch's fleet MCP setup.
type FleetMCPParams struct {
	// Dir is a directory inside the instance that belongs to this launch;
	// the strategy's files go under it.
	Dir string
	// Bridge is the argv, inside the instance, of a stdio MCP server that
	// relays to the fleet daemon.
	Bridge []string
}

// FleetMCPSetup is what one launch needs: files written into the instance
// before the agent starts, and environment set for the agent's process only.
type FleetMCPSetup struct {
	Files []File
	Env   []EnvVar
}

// File is one file to write into the instance.
type File struct {
	Path    string
	Content []byte
	Mode    os.FileMode
}

// EnvVar is one variable set for the launch.
type EnvVar struct {
	Name  string
	Value string
	// PathList puts Value in front of the variable's current value
	// (':'-separated) instead of replacing it, so a list the user set up
	// keeps its entries.
	PathList bool
}

// Exports renders the environment as a shell prelude for the launch command
// ("export A='x'; export B='y'${B:+:$B}; "), or "" when there is none.
func (s FleetMCPSetup) Exports() string {
	var b strings.Builder
	for _, v := range s.Env {
		b.WriteString("export ")
		b.WriteString(v.Name)
		b.WriteString("=")
		b.WriteString(shellquote.Single(v.Value))
		if v.PathList {
			b.WriteString("${" + v.Name + ":+:$" + v.Name + "}")
		}
		b.WriteString("; ")
	}
	return b.String()
}

// For returns the strategy for tool.
func For(tool state.AgentTool) Strategy {
	switch tool {
	case state.AgentToolClaude:
		return claudeStrategy{}
	default:
		return unsupportedStrategy{tool: tool}
	}
}

// ForCommand returns the strategy for the agent an automation agent's launch
// command runs (see ToolForCommand); an unrecognized command gets one that
// supports nothing.
func ForCommand(command string) Strategy {
	tool, _ := ToolForCommand(command)
	return For(tool)
}

// unsupportedStrategy is an agent fleet has no integration for (yet).
type unsupportedStrategy struct{ tool state.AgentTool }

func (u unsupportedStrategy) Tool() state.AgentTool { return u.tool }

func (unsupportedStrategy) SupportsFleetMCP() bool { return false }

func (unsupportedStrategy) FleetMCP(FleetMCPParams) (FleetMCPSetup, bool) {
	return FleetMCPSetup{}, false
}

// FleetMCPUnsupported says why an agent launched with command on backend would
// run WITHOUT the fleet MCP, or "" when its config can take it. The reason is
// a short predicate for "the fleet MCP ..." (it has to fit one line of the
// agent dialog). Config surfaces use it to warn when the toggle is turned on;
// the daemon adds its own host check on top (server agentFleetMCPProblem) to
// decide whether an agent's instance is served the MCP at all.
func FleetMCPUnsupported(command string, backend fleet.BackendType) string {
	if strings.TrimSpace(command) == "" {
		command = fleet.DefaultAgentCommand // what normalization fills in
	}
	if backend != "" && backend != fleet.BackendDevcontainer {
		return "needs a devcontainer backend"
	}
	agent, ok := detectAgent(command)
	switch {
	case !ok:
		return "needs a `claude` command"
	case !For(agent.tool).SupportsFleetMCP():
		return "is Claude Code only so far"
	case agent.clearsEnv:
		return "can't pass sudo/doas/env -i"
	}
	return ""
}
