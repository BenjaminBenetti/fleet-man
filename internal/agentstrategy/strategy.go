// Package agentstrategy is how fleet integrates with each kind of coding agent
// that runs in its instances — Claude Code, Codex, Auggie, ... — as one
// Strategy per agent tool.
//
// A strategy knows how to hand its agent the fleet MCP server and the Fleet
// Admiral skill (issue #219) inside an instance whose fleet has the Fleet MCP
// setting on. It must not write to the agent's own config (~/.claude,
// ~/.claude.json, ...): that is the user's, often shared across the fleet's
// instances, and has to read the same whether the setting is on or off. So a
// strategy writes what it needs into a directory of its own and points the
// agent at it through the environment of the instance's shells (fleet.rc runs
// `fleet mcp-env`, which calls InstallFleetMCP). Only Claude Code implements
// it so far; the others report that they don't support it.
package agentstrategy

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// Strategy integrates fleet with one kind of coding agent.
type Strategy interface {
	// Tool names the agent this strategy is for.
	Tool() state.AgentTool
	// SupportsFleetMCP reports whether this agent can be handed the fleet MCP.
	SupportsFleetMCP() bool
	// FleetMCP returns what gives the agent the fleet MCP server and the
	// Fleet Admiral skill, or ok=false when this agent has no way to take
	// them yet (or p is incomplete).
	FleetMCP(p FleetMCPParams) (setup FleetMCPSetup, ok bool)
}

// FleetMCPParams describes where a fleet MCP setup goes.
type FleetMCPParams struct {
	// Dir is a directory inside the instance that fleet owns; the strategy's
	// files go under it.
	Dir string
	// Bridge is the argv, inside the instance, of a stdio MCP server that
	// relays to the fleet daemon.
	Bridge []string
}

// FleetMCPSetup is what an agent needs: files written into the instance, and
// environment for the shells agents are started from.
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

// EnvVar is one variable to export.
type EnvVar struct {
	Name  string
	Value string
	// PathList adds Value to the front of the variable's current value
	// (':'-separated) instead of replacing it, so a list the user set up
	// keeps its entries — and does nothing when Value is already in it.
	PathList bool
}

// Exports renders the environment as shell code to eval, or "" when there is
// none. Sourcing it twice (a shell started from a shell) changes nothing.
func (s FleetMCPSetup) Exports() string {
	var b strings.Builder
	for _, v := range s.Env {
		value := shellquote.Single(v.Value)
		if v.PathList {
			// Skip when the list already holds it.
			b.WriteString(`case ":${` + v.Name + `:-}:" in *:` + value + `:*) ;; *) export ` +
				v.Name + `=` + value + `"${` + v.Name + `:+:$` + v.Name + `}" ;; esac` + "\n")
			continue
		}
		b.WriteString("export " + v.Name + "=" + value + "\n")
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

// tools are the agents fleet knows, in the order their setups are installed.
var tools = []state.AgentTool{
	state.AgentToolClaude,
	state.AgentToolCodex,
	state.AgentToolGemini,
	state.AgentToolCopilot,
	state.AgentToolAuggie,
}

// InstallFleetMCP writes, under dir, what every agent that can take the fleet
// MCP needs to load it, and returns the shell code that points those agents at
// it (FleetMCPSetup.Exports). It runs inside the instance, at the start of
// every shell there: a file is only rewritten when its content changed, and
// atomically, so shells starting at the same time never see half a file.
func InstallFleetMCP(dir string, bridge []string) (string, error) {
	var exports strings.Builder
	for _, tool := range tools {
		setup, ok := For(tool).FleetMCP(FleetMCPParams{Dir: dir, Bridge: bridge})
		if !ok {
			continue
		}
		for _, f := range setup.Files {
			if err := writeIfChanged(f); err != nil {
				return "", err
			}
		}
		exports.WriteString(setup.Exports())
	}
	return exports.String(), nil
}

// writeIfChanged writes f unless a file with that content is already there.
func writeIfChanged(f File) error {
	if current, err := os.ReadFile(f.Path); err == nil && bytes.Equal(current, f.Content) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}
	return atomicfile.Write(f.Path, f.Content, f.Mode)
}

// unsupportedStrategy is an agent fleet has no integration for (yet).
type unsupportedStrategy struct{ tool state.AgentTool }

func (u unsupportedStrategy) Tool() state.AgentTool { return u.tool }

func (unsupportedStrategy) SupportsFleetMCP() bool { return false }

func (unsupportedStrategy) FleetMCP(FleetMCPParams) (FleetMCPSetup, bool) {
	return FleetMCPSetup{}, false
}
