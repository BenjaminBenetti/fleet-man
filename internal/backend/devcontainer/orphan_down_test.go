package devcontainer

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

var _ backend.OrphanDowner = (*DevcontainerBackend)(nil)

func TestDownByKeyLooksUpTheBoundWorkspace(t *testing.T) {
	var asked []string
	listErr := errors.New("docker is down")
	workspaceContainers = func(dir string) ([]string, error) {
		asked = append(asked, dir)
		return nil, listErr
	}
	t.Cleanup(func() { workspaceContainers = dockerWorkspaceContainers })

	// Unbound: nothing to look for, docker never asked.
	if err := New().DownByKey(); err != nil || len(asked) != 0 {
		t.Fatalf("unbound DownByKey = %v, asked docker about %v", err, asked)
	}

	b := New()
	b.BindWorkspace("/home/u/.fleet/workspaces/alpha/i1/alpha")
	if err := b.DownByKey(); !errors.Is(err, listErr) {
		t.Errorf("DownByKey = %v, want the lookup's error", err)
	}
	if len(asked) != 1 || asked[0] != "/home/u/.fleet/workspaces/alpha/i1/alpha" {
		t.Errorf("looked up %v, want the bound workspace", asked)
	}

	// No docker on the host: nothing can be left.
	workspaceContainers = func(string) ([]string, error) { return nil, &exec.Error{Name: "docker", Err: exec.ErrNotFound} }
	if err := b.DownByKey(); err != nil {
		t.Errorf("DownByKey without docker = %v, want nil", err)
	}

	// A workspace with nothing left: nothing removed, no error.
	workspaceContainers = func(string) ([]string, error) { return nil, nil }
	if err := b.DownByKey(); err != nil {
		t.Errorf("DownByKey with no containers = %v", err)
	}
}
