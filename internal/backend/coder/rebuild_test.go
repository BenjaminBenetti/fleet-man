package coder

import (
	"strings"
	"testing"
)

// Coder workspaces have no rebuild primitive, so the backend must advertise the
// capability as unsupported and reject Rebuild with a clear error — the signal
// the CLI/TUI/MCP layers gate on.
func TestCoderRebuildUnsupported(t *testing.T) {
	b := New()
	if b.SupportsRebuild() {
		t.Fatal("coder backend should not support rebuild")
	}
	_, err := b.Rebuild("ws", "/tmp/ws", nil)
	if err == nil || !strings.Contains(err.Error(), "does not support rebuild") {
		t.Fatalf("Rebuild error = %v, want a 'does not support rebuild' error", err)
	}
}
