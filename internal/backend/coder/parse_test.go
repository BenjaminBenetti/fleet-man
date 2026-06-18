package coder

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

func TestParseToolProbeOutput(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantTool string
		wantOK   bool
	}{
		{"claude", "claude\n", "claude", true},
		{"copilot", "copilot\n", "copilot", true},
		{"codex", "codex\n", "codex", true},
		{"gemini", "gemini\n", "gemini", true},
		{"auggie", "auggie\n", "auggie", true},
		{"no-agent", "-\n", "", true},
		{"empty", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, ok := backend.ParseToolProbeOutput(tt.input)
			if tool != tt.wantTool || ok != tt.wantOK {
				t.Errorf("ParseToolProbeOutput(%q) = (%q, %v), want (%q, %v)",
					tt.input, tool, ok, tt.wantTool, tt.wantOK)
			}
		})
	}
}

func TestCoderWorkspaceName(t *testing.T) {
	tests := []struct {
		name      string
		wsDir     string
		wantName  string
	}{
		{
			"normal",
			"/home/user/.fleet/workspaces/fleet-man/agent1/fleet-man",
			"fleet-man-agent1",
		},
		{
			"dots in name",
			"/home/user/.fleet/workspaces/my.repo/test.1/my.repo",
			"my-repo-test-1",
		},
		{
			"uppercase",
			"/home/user/.fleet/workspaces/MyFleet/MyInst/MyFleet",
			"myfleet-myinst",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coderWorkspaceName(tt.wsDir)
			if got != tt.wantName {
				t.Errorf("coderWorkspaceName(%q) = %q, want %q", tt.wsDir, got, tt.wantName)
			}
		})
	}
}

func TestSanitizeCoderName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"fleet-man-agent1", "fleet-man-agent1"},
		{"My.Repo-Test", "my-repo-test"},
		{"---foo---", "foo"},
		{"UPPER_CASE", "upper-case"},
		{"a", "a"},
		{"", "workspace"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeCoderName(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeCoderName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
