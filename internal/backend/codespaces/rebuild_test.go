package codespaces

import (
	"strings"
	"testing"
)

// `gh codespace rebuild` recreates the dev container, so the codespaces backend
// advertises rebuild support.
func TestCodespacesSupportsRebuild(t *testing.T) {
	if !New().SupportsRebuild() {
		t.Fatal("codespaces backend should support rebuild")
	}
}

// Rebuild needs the codespace name (the container id) to target `gh codespace
// rebuild -c`; an empty id is rejected before shelling out.
func TestCodespacesRebuildRequiresName(t *testing.T) {
	_, err := New().Rebuild("", "/tmp/ws", nil)
	if err == nil || !strings.Contains(err.Error(), "codespace name") {
		t.Fatalf("Rebuild(\"\") error = %v, want a missing-name error", err)
	}
}
