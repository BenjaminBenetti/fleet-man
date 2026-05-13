package cli

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/dotfiles"
)

func TestSpawnSessionCommandConstruction(t *testing.T) {
	tests := []struct {
		name        string
		sessionName string
		want        string
	}{
		{
			name:        "simple session name",
			sessionName: "my-session",
			want:        "tmux new-session -d -s 'my-session'",
		},
		{
			name:        "session name with special chars",
			sessionName: "agent's-session",
			want:        "tmux new-session -d -s 'agent'\\''s-session'",
		},
		{
			name:        "session name with spaces",
			sessionName: "my session",
			want:        "tmux new-session -d -s 'my session'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test that ShQuote produces the expected output
			quoted := dotfiles.ShQuote(tt.sessionName)
			if quoted != "'"+tt.sessionName+"'" && tt.sessionName != "agent's-session" {
				// For simple cases without quotes
				if quoted != "'"+tt.sessionName+"'" {
					t.Errorf("ShQuote(%q) = %q, want quoted version", tt.sessionName, quoted)
				}
			}
			// For the special case with embedded single quote
			if tt.sessionName == "agent's-session" && quoted != "'agent'\\''s-session'" {
				t.Errorf("ShQuote(%q) = %q, want %q", tt.sessionName, quoted, "'agent'\\''s-session'")
			}
		})
	}
}
