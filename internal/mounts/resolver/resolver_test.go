package resolver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// TestResolveReturnsNilWhenNoSettingsEnabled verifies that an empty
// FleetSettings produces no mounts and no host-side side effects.
func TestResolveReturnsNilWhenNoSettingsEnabled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mounts, err := Resolve("my-fleet", fleet.FleetSettings{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if mounts != nil {
		t.Fatalf("Resolve() = %v, want nil", mounts)
	}
}

// TestResolveCreatesHostDirsAndReturnsMounts checks that an enabled
// setting both creates the host directory and returns a mount entry
// pointing at it.
func TestResolveCreatesHostDirsAndReturnsMounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	mounts, err := Resolve("alpha", fleet.FleetSettings{ClaudeCodeMount: true, CodexMount: true})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("len(mounts) = %d, want 2", len(mounts))
	}

	wantClaudeHost := filepath.Join(home, ".fleet", "workspaces", "alpha", ".claude")
	wantCodexHost := filepath.Join(home, ".fleet", "workspaces", "alpha", ".codex")

	byContainerPath := map[string]string{}
	for _, mount := range mounts {
		byContainerPath[mount.ContainerPath] = mount.LocalPath
	}

	if got := byContainerPath["/home/vscode/.claude"]; got != wantClaudeHost {
		t.Errorf("claude host path = %q, want %q", got, wantClaudeHost)
	}
	if got := byContainerPath["/home/vscode/.codex"]; got != wantCodexHost {
		t.Errorf("codex host path = %q, want %q", got, wantCodexHost)
	}

	for _, hostPath := range []string{wantClaudeHost, wantCodexHost} {
		info, err := os.Stat(hostPath)
		if err != nil {
			t.Errorf("expected host dir %s to exist: %v", hostPath, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", hostPath)
		}
	}
}

// TestResolveUsesHomeDirSetting verifies that an explicit HomeDir
// reroutes the container side of every mount under that path.
func TestResolveUsesHomeDirSetting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mounts, err := Resolve("alpha", fleet.FleetSettings{
		ClaudeCodeMount: true,
		HomeDir:         "/root",
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("len(mounts) = %d, want 1", len(mounts))
	}
	if mounts[0].ContainerPath != "/root/.claude" {
		t.Errorf("ContainerPath = %q, want %q", mounts[0].ContainerPath, "/root/.claude")
	}
}

// TestResolveOnlyEnabledMountsAreReturned ensures disabled toggles do
// not produce mounts and do not create their host directories.
func TestResolveOnlyEnabledMountsAreReturned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	mounts, err := Resolve("beta", fleet.FleetSettings{ClaudeCodeMount: true})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("len(mounts) = %d, want 1", len(mounts))
	}
	if mounts[0].ContainerPath != "/home/vscode/.claude" {
		t.Errorf("ContainerPath = %q, want %q", mounts[0].ContainerPath, "/home/vscode/.claude")
	}

	codexHost := filepath.Join(home, ".fleet", "workspaces", "beta", ".codex")
	if _, err := os.Stat(codexHost); !os.IsNotExist(err) {
		t.Errorf("expected codex host dir to be absent when disabled, stat err = %v", err)
	}
}
