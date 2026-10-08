package agentstrategy

import (
	"encoding/json"
	"path"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/admiralskill"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
)

// Claude Code takes the fleet MCP server and the Fleet Admiral skill as an
// inline PLUGIN: a directory holding a manifest, an .mcp.json and a skills/
// folder, named in CLAUDE_CODE_PLUGIN_DIRS (the environment form of
// --plugin-dir). Claude loads it for the processes that have the variable —
// nothing is written to ~/.claude or ~/.claude.json, and the command the user
// runs is left alone. Claude names the server "plugin:fleet:fleet" and the
// skill "fleet:fleet-admiral".

const (
	// claudePluginName is the plugin's name, and the MCP server's inside it.
	claudePluginName = "fleet"
	// claudePluginDirsEnv lists the inline plugin directories Claude loads.
	claudePluginDirsEnv = "CLAUDE_CODE_PLUGIN_DIRS"
)

type claudeStrategy struct{}

// pluginVersion is fleet's version as the semver a plugin manifest takes.
func pluginVersion() string {
	if v := strings.TrimPrefix(version.Version, "v"); v != "" {
		return v
	}
	return "0.0.0-dev"
}

func (claudeStrategy) Tool() state.AgentTool { return state.AgentToolClaude }

func (claudeStrategy) SupportsFleetMCP() bool { return true }

func (claudeStrategy) FleetMCP(p FleetMCPParams) (Setup, bool) {
	if p.Dir == "" || len(p.Bridge) == 0 {
		return Setup{}, false
	}
	root := path.Join(p.Dir, "claude-plugin")

	manifest, err := json.MarshalIndent(map[string]any{
		"name":        claudePluginName,
		"version":     pluginVersion(),
		"description": "The fleet MCP server and the Fleet Admiral skill, provided by fleet to the agents in this instance.",
		"author":      map[string]string{"name": "fleet-man"},
	}, "", "  ")
	if err != nil {
		return Setup{}, false
	}
	servers, err := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			claudePluginName: map[string]any{
				"command": p.Bridge[0],
				"args":    append([]string{}, p.Bridge[1:]...),
			},
		},
	}, "", "  ")
	if err != nil {
		return Setup{}, false
	}

	return Setup{
		Files: []File{
			{Path: path.Join(root, ".claude-plugin", "plugin.json"), Content: append(manifest, '\n'), Mode: 0o644},
			{Path: path.Join(root, ".mcp.json"), Content: append(servers, '\n'), Mode: 0o644},
			{Path: path.Join(root, "skills", admiralskill.Name, "SKILL.md"), Content: admiralskill.Content(), Mode: 0o644},
		},
		Env: []EnvVar{{Name: claudePluginDirsEnv, Value: root, PathList: true}},
	}, true
}
