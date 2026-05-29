package state

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
)

// TestControlSocketPath verifies the host control-socket path is rooted under
// the workspaces tree and ends with the per-instance .control dir plus the
// shared socket basename (so host and container agree on the same file).
func TestControlSocketPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const (
		fleetName    = "myfleet"
		instanceName = "alice"
	)

	got := ControlSocketPath(fleetName, instanceName)

	// Lives under the workspaces tree so it shares the instance's lifecycle.
	if !strings.HasPrefix(got, WorkspacesDir()+string(filepath.Separator)) {
		t.Errorf("ControlSocketPath = %q, want it under WorkspacesDir() %q", got, WorkspacesDir())
	}

	// Ends with .control/<SocketName>, basename sourced from control.SocketName.
	wantSuffix := filepath.Join(".control", control.SocketName)
	if !strings.HasSuffix(got, string(filepath.Separator)+wantSuffix) {
		t.Errorf("ControlSocketPath = %q, want suffix %q", got, wantSuffix)
	}

	// The socket sits directly inside ControlDir for the same instance.
	if dir := ControlDir(fleetName, instanceName); filepath.Dir(got) != dir {
		t.Errorf("filepath.Dir(socket) = %q, want ControlDir %q", filepath.Dir(got), dir)
	}

	// And ControlDir is the per-instance .control directory.
	wantDir := filepath.Join(WorkspacesDir(), fleetName, instanceName, ".control")
	if dir := ControlDir(fleetName, instanceName); dir != wantDir {
		t.Errorf("ControlDir = %q, want %q", dir, wantDir)
	}
}
