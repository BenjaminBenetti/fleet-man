package agentstrategy

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/admiralskill"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

func TestToolForCommand(t *testing.T) {
	cases := []struct {
		command string
		want    state.AgentTool
		ok      bool
	}{
		{"claude --system-prompt '${SYS_PROMPT}' '${PROMPT}'", state.AgentToolClaude, true},
		{"IS_SANDBOX=1 claude --dangerously-skip-permissions '${PROMPT}'", state.AgentToolClaude, true},
		{"cd app && ~/.local/bin/claude '${PROMPT}'", state.AgentToolClaude, true},
		{"env FOO=1 /usr/local/bin/codex exec \"${PROMPT}\"", state.AgentToolCodex, true},
		{"auggie --print '${PROMPT}'", state.AgentToolAuggie, true},
		{"(gemini -p '${PROMPT}')", state.AgentToolGemini, true},
		// An agent named inside a quoted argument is not the command.
		{"./run.sh --tool 'claude' \"codex\"", "", false},
		{"./run.sh c\\laude", "", false},
		{"./run-agent.sh '${PROMPT}'", "", false},
		{"", "", false},
		// The first agent word wins.
		{"claude -p \"$(codex --version)\"", state.AgentToolClaude, true},
		{"npx -y @anthropic-ai/claude-code@latest '${PROMPT}'", state.AgentToolClaude, true},
		{"exec env -i HOME=/h claude", state.AgentToolClaude, true},
		{"sudo -u vscode claude x", state.AgentToolClaude, true},
		{"echo start | claude -p x", state.AgentToolClaude, true},
		// Assignments with quoted values, and wrappers with their own flags,
		// operands and flag values.
		{"ANTHROPIC_MODEL=\"claude-opus-5-5\" claude -p '${PROMPT}'", state.AgentToolClaude, true},
		{"GH_TOKEN=\"$(cat ~/.gh-token)\" claude -p x", state.AgentToolClaude, true},
		{"IS_SANDBOX=1 MAX_THINKING_TOKENS='8000' ~/.local/bin/claude x", state.AgentToolClaude, true},
		{"env FOO=\"a b\" claude x", state.AgentToolClaude, true},
		{"timeout 2h claude -p x", state.AgentToolClaude, true},
		{"timeout -k 30s --signal=TERM 2h claude -p x", state.AgentToolClaude, true},
		{"nice -n 10 claude -p x", state.AgentToolClaude, true},
		{"env -u CI claude -p x", state.AgentToolClaude, true},
		{"time -o f claude -p x", state.AgentToolClaude, true},
		{"IS_SANDBOX=1 timeout 2h nice claude x", state.AgentToolClaude, true},
		{"timeout \"$AGENT_TIMEOUT\" claude -p x", state.AgentToolClaude, true},
		{"timeout -s KILL '2h' claude -p x", state.AgentToolClaude, true},
		// Redirections are not command words.
		{">/tmp/agent.log claude -p x", state.AgentToolClaude, true},
		{"2>/dev/null claude -p x", state.AgentToolClaude, true},
		{"claude -p x 2>&1 | tee /tmp/log", state.AgentToolClaude, true},
		{">&2 claude -p x", state.AgentToolClaude, true},
		{"./run.sh >claude.log", "", false},
		{"./run.sh &>/dev/null claude", "", false},
		{"./run.sh &> out.log claude", "", false},
		{"echo hi >| out.log claude", "", false},
		{"&>/dev/null claude -p x", state.AgentToolClaude, true},
		{"claude -p x &>/dev/null", state.AgentToolClaude, true},
		{"./run.sh |& claude", state.AgentToolClaude, true},
		{"nice -n10 claude x", state.AgentToolClaude, true},
		{"timeout -k30s 2h claude x", state.AgentToolClaude, true},
		// A quoted command word is not recognized.
		{"\"claude\" -p x", "", false},
		{"timeout 2h \"claude\" -p x", "", false},
		// An agent's name in an argument is not the command.
		{"cd /workspaces/claude-code && ./run-agent.sh", "", false},
		{"./run-agent.sh --workdir /src/claude-code", "", false},
		{"./run.sh claude", "", false},
		{"bash -lc 'claude ${PROMPT}'", "", false},
	}
	for _, c := range cases {
		got, ok := ToolForCommand(c.command)
		if got != c.want || ok != c.ok {
			t.Errorf("ToolForCommand(%q) = %q, %v; want %q, %v", c.command, got, ok, c.want, c.ok)
		}
	}
}

func TestForCommandPicksTheStrategy(t *testing.T) {
	if claude := ForCommand("claude '${PROMPT}'"); claude.Tool() != state.AgentToolClaude || !claude.SupportsFleetMCP() {
		t.Fatalf("claude command: tool = %q, supports = %v", claude.Tool(), claude.SupportsFleetMCP())
	}
	s := ForCommand("codex '${PROMPT}'")
	if s.Tool() != state.AgentToolCodex || s.SupportsFleetMCP() {
		t.Fatalf("codex command: tool = %q, supports = %v", s.Tool(), s.SupportsFleetMCP())
	}
	if _, ok := s.FleetMCP(FleetMCPParams{Dir: "/tmp/x", Bridge: []string{"fleet", "mcp-bridge"}}); ok {
		t.Fatal("codex has no fleet MCP integration yet")
	}
	if _, ok := ForCommand("./wrapper.sh").FleetMCP(FleetMCPParams{Dir: "/tmp/x", Bridge: []string{"fleet"}}); ok {
		t.Fatal("an unrecognized command must not get a fleet MCP setup")
	}
}

func TestClaudeFleetMCPIsAnInlinePlugin(t *testing.T) {
	setup, ok := For(state.AgentToolClaude).FleetMCP(FleetMCPParams{
		Dir:    "/tmp/fleet-mcp",
		Bridge: []string{"/usr/bin/fleet", "mcp-bridge"},
	})
	if !ok {
		t.Fatal("claude supports the fleet MCP")
	}
	files := map[string]File{}
	for _, f := range setup.Files {
		files[f.Path] = f
	}
	root := "/tmp/fleet-mcp/claude-plugin"

	var manifest struct{ Name, Version string }
	if err := json.Unmarshal(files[root+"/.claude-plugin/plugin.json"].Content, &manifest); err != nil || manifest.Name != "fleet" || manifest.Version == "" {
		t.Fatalf("plugin manifest = %+v (%v)", manifest, err)
	}
	var servers struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(files[root+"/.mcp.json"].Content, &servers); err != nil {
		t.Fatal(err)
	}
	fleetServer, ok := servers.MCPServers["fleet"]
	if !ok || fleetServer.Command != "/usr/bin/fleet" || len(fleetServer.Args) != 1 || fleetServer.Args[0] != "mcp-bridge" {
		t.Fatalf(".mcp.json servers = %+v", servers.MCPServers)
	}
	skill := files[root+"/skills/fleet-admiral/SKILL.md"]
	if !bytes.Equal(skill.Content, admiralskill.Content()) {
		t.Fatal("the plugin must carry the Fleet Admiral skill")
	}
	for path, f := range files {
		if f.Mode != 0o644 {
			t.Errorf("%s mode = %v, want 0644", path, f.Mode)
		}
	}
	if len(setup.Env) != 1 || setup.Env[0].Name != "CLAUDE_CODE_PLUGIN_DIRS" || setup.Env[0].Value != root || !setup.Env[0].PathList {
		t.Fatalf("env = %+v, want CLAUDE_CODE_PLUGIN_DIRS=%s prepended", setup.Env, root)
	}
}

func TestClaudeFleetMCPNeedsADirAndABridge(t *testing.T) {
	if _, ok := For(state.AgentToolClaude).FleetMCP(FleetMCPParams{Bridge: []string{"fleet"}}); ok {
		t.Fatal("no dir: nothing to point the agent at")
	}
	if _, ok := For(state.AgentToolClaude).FleetMCP(FleetMCPParams{Dir: "/tmp/x"}); ok {
		t.Fatal("no bridge: nothing to run")
	}
}

// TestExportsInAShell runs the rendered prelude through sh: values survive
// quoting, and a path list keeps what the user already had.
func TestExportsInAShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	setup := FleetMCPSetup{Env: []EnvVar{
		{Name: "FLEET_TEST_PLAIN", Value: "it's $HOME"},
		{Name: "FLEET_TEST_LIST", Value: "/tmp/fleet mcp/plugin", PathList: true},
	}}
	script := setup.Exports() + `printf '%s|%s' "$FLEET_TEST_PLAIN" "$FLEET_TEST_LIST"`

	run := func(env ...string) string {
		cmd := exec.Command(sh, "-c", script)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if got := run("FLEET_TEST_LIST="); got != "it's $HOME|/tmp/fleet mcp/plugin" {
		t.Fatalf("unset list: %q", got)
	}
	if got := run("FLEET_TEST_LIST=/home/me/plugins"); got != "it's $HOME|/tmp/fleet mcp/plugin:/home/me/plugins" {
		t.Fatalf("existing list: %q", got)
	}
	if (FleetMCPSetup{}).Exports() != "" {
		t.Fatal("no env: no prelude")
	}
}

// TestClaudeLoadsThePlugin checks against a real Claude Code that it picks the
// plugin's MCP server up from CLAUDE_CODE_PLUGIN_DIRS alone. Opt-in
// (FLEET_TEST_CLAUDE=1): `claude mcp list` also health-checks every MCP server
// the user has configured.
func TestClaudeLoadsThePlugin(t *testing.T) {
	if os.Getenv("FLEET_TEST_CLAUDE") != "1" {
		t.Skip("set FLEET_TEST_CLAUDE=1 to check against the installed claude")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not installed")
	}
	dir := t.TempDir()
	setup, _ := For(state.AgentToolClaude).FleetMCP(FleetMCPParams{Dir: dir, Bridge: []string{"/bin/true"}})
	for _, f := range setup.Files {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.Path, f.Content, f.Mode); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", setup.Exports()+"exec "+claude+" mcp list")
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "plugin:fleet:fleet") {
		t.Fatalf("claude did not load the plugin's MCP server:\n%s", out)
	}
}

func TestFleetMCPUnsupported(t *testing.T) {
	cases := []struct {
		command string
		backend fleet.BackendType
		want    string // substring; "" means supported
	}{
		{"claude '${PROMPT}'", fleet.BackendDevcontainer, ""},
		{"", "", ""}, // the default command, the default backend
		{"npx -y @anthropic-ai/claude-code@latest '${PROMPT}'", fleet.BackendDevcontainer, ""},
		{"claude '${PROMPT}'", fleet.BackendCoder, "devcontainer backend"},
		{"codex '${PROMPT}'", fleet.BackendDevcontainer, "Claude Code only"},
		{"./agent.sh", fleet.BackendDevcontainer, "`claude` command"},
		// Recognized, but the wrapper clears what the launch exports.
		{"env -i HOME=/h claude x", fleet.BackendDevcontainer, "sudo/doas/env -i"},
		{"sudo -u vscode claude x", fleet.BackendDevcontainer, "sudo/doas/env -i"},
		{"sudo --preserve-env=HOME claude x", fleet.BackendDevcontainer, "sudo/doas/env -i"},
		{"exec -cl claude x", fleet.BackendDevcontainer, "sudo/doas/env -i"},
		{"env -vi claude x", fleet.BackendDevcontainer, "sudo/doas/env -i"},
		{"env -iu CI claude x", fleet.BackendDevcontainer, "sudo/doas/env -i"},
		{"env - claude x", fleet.BackendDevcontainer, "sudo/doas/env -i"},
		// sudo -E keeps the environment (or refuses to run at all).
		{"sudo -E -u vscode claude x", fleet.BackendDevcontainer, ""},
		{"sudo -Eu vscode claude x", fleet.BackendDevcontainer, ""},
		{"sudo --preserve-env claude x", fleet.BackendDevcontainer, ""},
		{"env -uCI claude x", fleet.BackendDevcontainer, ""},
		// A keep flag only undoes its own launcher's clear.
		{"env -i sudo -E claude x", fleet.BackendDevcontainer, "sudo/doas/env -i"},
		{"sudo -E env -i claude x", fleet.BackendDevcontainer, "sudo/doas/env -i"},
		{"sudo -u vscode -E claude x", fleet.BackendDevcontainer, ""},
		{"doas claude x", fleet.BackendDevcontainer, "sudo/doas/env -i"},
		{"env -u CI claude x", fleet.BackendDevcontainer, ""},
	}
	for _, c := range cases {
		got := FleetMCPUnsupported(c.command, c.backend)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("FleetMCPUnsupported(%q, %q) = %q, want %q", c.command, c.backend, got, c.want)
		}
		// It must fit one line of the agent dialog: "Won't apply: <why>."
		if n := len("Won't apply: " + got + "."); n > 42 {
			t.Errorf("reason %q makes a %d-char dialog line, want <= 42", got, n)
		}
	}
}
