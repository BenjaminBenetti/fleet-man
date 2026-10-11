package fleet

import "testing"

func TestAgentLoginTool(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    string // "" = not a login tool
	}{
		{DefaultAgentCommand, "claude"},
		{"claude", "claude"},
		{"  /usr/local/bin/claude -p '${PROMPT}'", "claude"},
		{"ANTHROPIC_API_KEY=sk-1 claude '${PROMPT}'", "claude"},
		{"FOO='a b' BAR=\"c d\" codex exec '${PROMPT}'", "codex"},
		{"env -u HOME -i PATH=/bin auggie --print '${PROMPT}'", "auggie"},
		{"exec claude", "claude"},
		{"nohup codex", "codex"},
		{"'claude' --x", "claude"},
		{"cd /work && claude", ""},
		{"bash -lc 'claude'", ""},
		{"echo claude", ""},
		{"claudette", ""},
		{"gemini '${PROMPT}'", ""},
		{"", ""},
		{"A=1", ""},
		{"2X=1 claude", ""},
	} {
		got, ok := AgentLoginTool(tc.command)
		if (tc.want == "") == ok || got.Tool != tc.want {
			t.Errorf("AgentLoginTool(%q) = %q, %v; want %q", tc.command, got.Tool, ok, tc.want)
		}
	}
}

func TestLoginToolSetBy(t *testing.T) {
	claude, _ := LoginToolNamed("claude")
	codex, _ := LoginToolNamed("codex")
	for _, tc := range []struct {
		tool    LoginTool
		command string
		want    bool
	}{
		{claude, "ANTHROPIC_API_KEY=x claude", true},
		{claude, "export CLAUDE_CODE_OAUTH_TOKEN=t; claude", true},
		{claude, "env CLAUDE_CODE_USE_BEDROCK=1 claude", true},
		{claude, "claude --api-key-helper x", false},
		{claude, "MY_ANTHROPIC_API_KEY=x claude", false},
		{claude, "OPENAI_API_KEY=x claude", false},
		{codex, "OPENAI_API_KEY=x codex", true},
		{codex, "(CODEX_API_KEY=y codex)", true},
		{codex, "codex", false},
	} {
		if got := tc.tool.SetBy(tc.command); got != tc.want {
			t.Errorf("%s.SetBy(%q) = %v, want %v", tc.tool.Tool, tc.command, got, tc.want)
		}
	}
	if !claude.SetIn(map[string]bool{"PATH": true, "ANTHROPIC_API_KEY": true}) || claude.SetIn(map[string]bool{"OPENAI_API_KEY": true}) {
		t.Error("SetIn does not follow the tool's key variables")
	}
}

func TestLoginToolsAreComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range LoginTools() {
		if tool.Tool == "" || tool.Name == "" || tool.MountName == "" || tool.LoginCommand == "" || len(tool.KeyVars) == 0 {
			t.Errorf("incomplete login tool %+v", tool)
		}
		if seen[tool.Tool] {
			t.Errorf("tool %s listed twice", tool.Tool)
		}
		seen[tool.Tool] = true
		if got, ok := LoginToolNamed(tool.Tool); !ok || got.Name != tool.Name {
			t.Errorf("LoginToolNamed(%q) = %+v, %v", tool.Tool, got, ok)
		}
	}
	if _, ok := LoginToolNamed("gemini"); ok {
		t.Error("gemini is not a login tool")
	}
}
