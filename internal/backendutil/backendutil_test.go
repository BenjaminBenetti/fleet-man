package backendutil

import (
	"testing"

	coderbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// TestNewForInstanceRegistersCoderName guards the coder name registration
// (issue #221): with a per-fleet workspace-name override the name is no
// longer derivable from the workspace path, so NewForInstance must seed the
// backend with the instance's recorded container ID. EditorURI resolves
// through that registration, making it observable without exec'ing coder.
func TestNewForInstanceRegistersCoderName(t *testing.T) {
	inst := &fleet.Instance{
		Name:         "agent-1",
		Backend:      fleet.BackendCoder,
		ContainerID:  "customname-agent-1.dev",
		WorkspaceDir: "/home/u/.fleet/workspaces/alpha/agent-1/alpha",
	}
	b := NewForInstance(inst, false)
	uri, ok := b.EditorURI(inst.WorkspaceDir, "alpha")
	if !ok || uri != "coder-vscode://customname-agent-1" {
		t.Fatalf("EditorURI = %q, %v; want the registered name, true", uri, ok)
	}

	// Without a recorded container ID (still creating / failed) there is
	// nothing to register — the historical path derivation must still hold.
	blank := &fleet.Instance{
		Name:         "agent-1",
		Backend:      fleet.BackendCoder,
		WorkspaceDir: "/home/u/.fleet/workspaces/alpha/agent-1/alpha",
	}
	if _, isCoder := NewForInstance(blank, false).(*coderbackend.CoderBackend); !isCoder {
		t.Fatal("expected a CoderBackend")
	}
	uri, ok = NewForInstance(blank, false).EditorURI(blank.WorkspaceDir, "alpha")
	if !ok || uri != "coder-vscode://alpha-agent-1" {
		t.Fatalf("EditorURI fallback = %q, %v; want path-derived alpha-agent-1, true", uri, ok)
	}
}
