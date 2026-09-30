package agentstrategy

import (
	"encoding/json"
	"path"

	"github.com/BenjaminBenetti/fleet-man/internal/admiralskill"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// Claude Code takes the fleet MCP server and the Fleet Admiral skill as an
// inline PLUGIN: a directory holding a manifest, an .mcp.json and a skills/
// folder, named in CLAUDE_CODE_PLUGIN_DIRS (the environment form of
// --plugin-dir). Loaded for that process only — nothing is written to the
// fleet's shared ~/.claude or ~/.claude.json, and the agent's own command line
// is left alone. Claude names the server "plugin:fleet:fleet" and the skill
// "fleet:fleet-admiral".

const (
	// claudePluginName is the plugin's name, and the MCP server's inside it.
	claudePluginName = "fleet"
	// claudePluginDirsEnv lists the inline plugin directories Claude loads.
	claudePluginDirsEnv = "CLAUDE_CODE_PLUGIN_DIRS"
)

type claudeStrategy struct{}

func (claudeStrategy) Tool() state.AgentTool { return state.AgentToolClaude }

func (claudeStrategy) FleetMCP(p FleetMCPParams) (FleetMCPSetup, bool) {
	if p.Dir == "" || len(p.Bridge) == 0 {
		return FleetMCPSetup{}, false
	}
	root := path.Join(p.Dir, "claude-plugin")

	manifest, err := json.MarshalIndent(map[string]string{
		"name":        claudePluginName,
		"description": "The fleet MCP server and the Fleet Admiral skill, given to this automation agent by fleet.",
	}, "", "  ")
	if err != nil {
		return FleetMCPSetup{}, false
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
		return FleetMCPSetup{}, false
	}

	return FleetMCPSetup{
		Files: []File{
			{Path: path.Join(root, ".claude-plugin", "plugin.json"), Content: append(manifest, '\n'), Mode: 0o644},
			{Path: path.Join(root, ".mcp.json"), Content: append(servers, '\n'), Mode: 0o644},
			{Path: path.Join(root, "skills", admiralskill.Name, "SKILL.md"), Content: admiralskill.Content(), Mode: 0o644},
		},
		Env: []EnvVar{{Name: claudePluginDirsEnv, Value: root, PathList: true}},
	}, true
}
