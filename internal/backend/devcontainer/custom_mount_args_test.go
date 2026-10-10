package devcontainer

import (
	"slices"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// TestCustomMountArgsBindsLocalPathForEveryScope pins that docker binds
// LocalPath whatever the scope: SubPath only matters to backends that keep
// fleet-shared state somewhere other than the daemon host.
func TestCustomMountArgsBindsLocalPathForEveryScope(t *testing.T) {
	mounts := []backend.Mount{
		{LocalPath: "/h/.fleet/workspaces/f/.claude", ContainerPath: "/home/vscode/.claude", Scope: backend.MountScopeFleetShared, SubPath: ".claude"},
		{LocalPath: "/h/.fleet/workspaces/f/i/.fleet-control", ContainerPath: "/fleet-mounts/control"},
		{LocalPath: "", ContainerPath: "/skipped"},
	}
	got, err := customMountArgs(mounts)
	if err != nil {
		t.Fatalf("customMountArgs() error = %v", err)
	}
	want := []string{
		"--mount", "type=bind,source=/h/.fleet/workspaces/f/.claude,target=/home/vscode/.claude",
		"--mount", "type=bind,source=/h/.fleet/workspaces/f/i/.fleet-control,target=/fleet-mounts/control",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("customMountArgs() = %q, want %q", got, want)
	}
}

func TestCustomMountArgsEmpty(t *testing.T) {
	got, err := customMountArgs(nil)
	if err != nil || got != nil {
		t.Fatalf("customMountArgs(nil) = %q, %v; want nil, nil", got, err)
	}
}

// TestCustomMountArgsRefusesReadOnly guards against a read-only request
// being mounted writable: `devcontainer up --mount` has no read-only form.
func TestCustomMountArgsRefusesReadOnly(t *testing.T) {
	_, err := customMountArgs([]backend.Mount{
		{LocalPath: "/h/data", ContainerPath: "/opt/data", ReadOnly: true},
	})
	if err == nil || !strings.Contains(err.Error(), "/opt/data") {
		t.Fatalf("customMountArgs(read-only) error = %v, want a refusal naming /opt/data", err)
	}
}
