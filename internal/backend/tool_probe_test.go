package backend

import "testing"

func TestParseToolProbeOutput(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		wantTool string
		wantOK   bool
	}{
		{"claude detected", "claude\n", "claude", true},
		{"copilot detected", "copilot\n", "copilot", true},
		{"codex detected", "codex\n", "codex", true},
		{"gemini detected", "gemini\n", "gemini", true},
		{"no agent", "-\n", "", true},
		{"empty output (exec failure)", "", "", false},
		{"whitespace only (exec failure)", "  \n", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, ok := ParseToolProbeOutput(tt.output)
			if ok != tt.wantOK {
				t.Fatalf("ParseToolProbeOutput(%q) ok = %v, want %v", tt.output, ok, tt.wantOK)
			}
			if tool != tt.wantTool {
				t.Fatalf("ParseToolProbeOutput(%q) tool = %q, want %q", tt.output, tool, tt.wantTool)
			}
		})
	}
}
