package devcontainer

import "testing"

// The devcontainer backend recreates a container in place, so it advertises
// rebuild support (the capability the CLI/TUI/MCP layers gate on).
func TestDevcontainerSupportsRebuild(t *testing.T) {
	if !New().SupportsRebuild() {
		t.Fatal("devcontainer backend should support rebuild")
	}
}
