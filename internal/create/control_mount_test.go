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

// TestControlMountWritesMarker: provisioning marks the control directory as
// mounted into the container — before the ControlDirReady hook opens the
// instance's sockets there, and idempotently across a rebuild — since the
// daemon also creates the directory for instances whose container lacks the
// mount, and the devcontainer backend tells them apart by this marker.
func TestControlMountWritesMarker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	marker := control.MountMarkerPath(state.ControlDir("f", "i"))

	orig := ControlDirReady
	t.Cleanup(func() { ControlDirReady = orig })
	hookSawMarker := false
	ControlDirReady = func(fleetName, instanceName string) {
		_, err := os.Stat(marker)
		hookSawMarker = err == nil
	}

	for range 2 {
		if _, err := controlMount("f", "i"); err != nil {
			t.Fatalf("controlMount: %v", err)
		}
		info, err := os.Stat(marker)
		if err != nil {
			t.Fatalf("marker %s not written: %v", marker, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("marker %s is not a regular file: %v", marker, info.Mode())
		}
		if !hookSawMarker {
			t.Fatal("ControlDirReady ran before the marker was written")
		}
	}
}
