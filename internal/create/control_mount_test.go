package create

import (
	"os"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestControlMount verifies that controlMount returns a bind mount pointing at
// the per-instance control directory inside the container at the well-known
// control.ContainerMountDir, that its host LocalPath matches
// state.ControlDir, and that the host directory is created as a side effect.
//
// Each case runs against a temp HOME (via t.Setenv) so state.ControlDir
// resolves into an isolated tree that the test owns and the testing harness
// cleans up.
func TestControlMount(t *testing.T) {
	tests := []struct {
		name     string
		fleet    string
		instance string
	}{
		{
			name:     "simple names",
			fleet:    "myfleet",
			instance: "alpha",
		},
		{
			name:     "hyphenated names",
			fleet:    "my-fleet",
			instance: "instance-2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())

			mount, err := controlMount(tt.fleet, tt.instance)
			if err != nil {
				t.Fatalf("controlMount(%q, %q) returned error: %v", tt.fleet, tt.instance, err)
			}

			if mount.ContainerPath != control.ContainerMountDir {
				t.Errorf("ContainerPath = %q, want %q", mount.ContainerPath, control.ContainerMountDir)
			}

			wantLocal := state.ControlDir(tt.fleet, tt.instance)
			if mount.LocalPath != wantLocal {
				t.Errorf("LocalPath = %q, want %q", mount.LocalPath, wantLocal)
			}

			info, err := os.Stat(wantLocal)
			if err != nil {
				t.Fatalf("host control dir %q not created: %v", wantLocal, err)
			}
			if !info.IsDir() {
				t.Errorf("host control path %q is not a directory", wantLocal)
			}
		})
	}
}
