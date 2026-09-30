package create

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	mountresolver "github.com/BenjaminBenetti/fleet-man/internal/mounts/resolver"
)

// Execute the actual provisioning script in an isolated local workspace.
type localMountBackend struct{ backend.Backend }

func (localMountBackend) ExecCommand(workspace string, command []string) *backend.Cmd {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = workspace
	return backend.NewCmd(cmd, nil)
}

func TestProjectSettingsSharedAcrossInstances(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "claude-settings.local.json")
	if err := os.WriteFile(shared, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := mountresolver.Symlink{Source: shared, Target: ".claude/settings.local.json", SeedContent: "{}"}
	first, second := t.TempDir(), t.TempDir()
	original := `{"permissions":{"allow":["Bash(go test:*)"]}}`
	if err := os.MkdirAll(filepath.Join(first, ".claude"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, link.Target), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	// Keep project-owned config next to the local overrides intact.
	tracked := filepath.Join(first, ".claude", "settings.json")
	if err := os.WriteFile(tracked, []byte("tracked"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{first, second} {
		if err := applyMountSymlinks(localMountBackend{}, ws, []mountresolver.Symlink{link}); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(ws, link.Target)); err != nil || string(got) != original {
			t.Fatalf("settings in %s = %s, %v", ws, got, err)
		}
	}
	updated := `{"permissions":{"allow":["Bash(go vet:*)"]}}`
	if err := os.WriteFile(filepath.Join(second, link.Target), []byte(updated), 0600); err != nil {
		t.Fatal(err)
	}
	// Rebuild/clone provisioning must preserve existing shared overrides.
	for _, ws := range []string{first, second, t.TempDir()} {
		if err := applyMountSymlinks(localMountBackend{}, ws, []mountresolver.Symlink{link}); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(ws, link.Target)); err != nil || string(got) != updated {
			t.Fatalf("shared settings = %s, %v", got, err)
		}
	}
	if got, err := os.ReadFile(tracked); err != nil || string(got) != "tracked" {
		t.Fatalf("project settings modified: %s, %v", got, err)
	}
}
