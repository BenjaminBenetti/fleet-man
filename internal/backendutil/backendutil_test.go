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

// TestBackendCapabilitiesMatchBackends keeps fleet.BackendType.Capabilities —
// the hints clients gate their clone and rebuild keys on — equal to what each
// backend itself reports, so a client never offers an action the server
// refuses, or hides one it would run.
func TestBackendCapabilitiesMatchBackends(t *testing.T) {
	for _, backendType := range []fleet.BackendType{fleet.BackendDevcontainer, fleet.BackendCoder, fleet.BackendCodespaces} {
		t.Run(string(backendType), func(t *testing.T) {
			b := CapabilitiesFor(&fleet.Instance{Backend: backendType})
			hints := backendType.Capabilities()
			if hints.Clone != b.SupportsClone() {
				t.Errorf("Capabilities().Clone = %v, backend SupportsClone() = %v", hints.Clone, b.SupportsClone())
			}
			if hints.Rebuild != b.SupportsRebuild() {
				t.Errorf("Capabilities().Rebuild = %v, backend SupportsRebuild() = %v", hints.Rebuild, b.SupportsRebuild())
			}
		})
	}
}

// TestCapabilitiesForAnswersByInstanceWithoutBinding pins the capability-only
// handle the server uses under the state lock: it answers for the instance's
// backend (an empty type meaning devcontainer, as NewForInstance does) and does
// not register the instance's name, so it does no I/O.
func TestCapabilitiesForAnswersByInstanceWithoutBinding(t *testing.T) {
	for _, tc := range []struct {
		backend         fleet.BackendType
		mounts, rebuild bool
		clone, micSink  bool
	}{
		{backend: "", mounts: true, rebuild: true, clone: true, micSink: true},
		{backend: fleet.BackendDevcontainer, mounts: true, rebuild: true, clone: true, micSink: true},
		{backend: fleet.BackendCodespaces, rebuild: true},
		{backend: fleet.BackendCoder},
	} {
		inst := &fleet.Instance{Name: "i", Backend: tc.backend, ContainerID: "c"}
		caps := CapabilitiesFor(inst)
		acting := NewForInstance(inst, false)
		if caps.SupportsCustomMounts() != tc.mounts || caps.SupportsRebuild() != tc.rebuild ||
			caps.SupportsClone() != tc.clone || caps.SupportsMicSink() != tc.micSink {
			t.Fatalf("CapabilitiesFor(%q) = mounts %v rebuild %v clone %v mic %v, want %+v", tc.backend,
				caps.SupportsCustomMounts(), caps.SupportsRebuild(), caps.SupportsClone(), caps.SupportsMicSink(), tc)
		}
		if caps.SupportsCustomMounts() != acting.SupportsCustomMounts() || caps.SupportsRebuild() != acting.SupportsRebuild() ||
			caps.SupportsClone() != acting.SupportsClone() || caps.SupportsMicSink() != acting.SupportsMicSink() {
			t.Fatalf("CapabilitiesFor(%q) disagrees with NewForInstance", tc.backend)
		}
	}

	// A coder instance with a recorded (non-derivable) workspace name: the
	// acting handle registers it, the capability handle must not.
	inst := &fleet.Instance{
		Name:         "agent-1",
		Backend:      fleet.BackendCoder,
		ContainerID:  "customname-agent-1.dev",
		WorkspaceDir: "/home/u/.fleet/workspaces/alpha/agent-1/alpha",
	}
	coder, ok := CapabilitiesFor(inst).(*coderbackend.CoderBackend)
	if !ok {
		t.Fatalf("CapabilitiesFor(coder) = %T, want *CoderBackend", CapabilitiesFor(inst))
	}
	if uri, _ := coder.EditorURI(inst.WorkspaceDir, "alpha"); uri != "coder-vscode://alpha-agent-1" {
		t.Fatalf("capability handle EditorURI = %q; it registered the instance's name", uri)
	}
}
