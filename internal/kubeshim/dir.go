package kubeshim

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
)

// Dir is a shim directory: the docker and docker-compose links to the fleet
// executable that the devcontainer CLI is pointed at, the kubeshim.json mode
// file beside them, and what the shim leaves there (the capture, the call
// journal, the exec socket). It is private to one CLI invocation (0700) and
// holds secrets while it lives (the capture's environment, pod mode's
// container environment), so Remove it when the invocation is over. Removing
// it also ends any shim still blocked in `docker events`.
type Dir struct {
	path string
}

// maxSocketPath bounds the exec socket's path: sun_path holds 104 bytes on
// macOS and the BSDs, 108 on Linux.
const maxSocketPath = 100

// NewDir creates a shim directory whose links point at exe, the fleet
// executable (fleetd's own, os.Executable). It is created under the system
// temp dir, or under /tmp when that is too deep for a unix socket path.
func NewDir(exe string) (*Dir, error) {
	if !filepath.IsAbs(exe) {
		return nil, fmt.Errorf("kubeshim: executable %q is not absolute", exe)
	}
	base := os.TempDir()
	// "<base>/fleet-kubeshim-<10 digits>/exec.sock"
	if len(base)+len("/fleet-kubeshim-0123456789/")+len(execSocketFile) > maxSocketPath {
		base = "/tmp"
	}
	path, err := os.MkdirTemp(base, "fleet-kubeshim-")
	if err != nil {
		return nil, err
	}
	d := &Dir{path: path}
	for _, name := range []string{dockerLink, composeLink} {
		if err := os.Symlink(exe, filepath.Join(path, name)); err != nil {
			_ = d.Remove()
			return nil, fmt.Errorf("kubeshim: linking %s: %w", name, err)
		}
	}
	return d, nil
}

// Path is the directory.
func (d *Dir) Path() string { return d.path }

// Docker is the path to give the devcontainer CLI as --docker-path.
func (d *Dir) Docker() string { return filepath.Join(d.path, dockerLink) }

// Compose is the path to give the devcontainer CLI as --docker-compose-path.
func (d *Dir) Compose() string { return filepath.Join(d.path, composeLink) }

// ExecSocket is where pod mode's exec socket goes.
func (d *Dir) ExecSocket() string { return filepath.Join(d.path, execSocketFile) }

// Configure writes the mode file. The shim reads it on every call, so it is
// written before the CLI starts.
func (d *Dir) Configure(c Config) error {
	if err := c.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(d.path, ConfigFile), data, 0o600)
}

// Capture returns the docker run the capture-mode shim captured, or
// ErrNoCapture when the CLI never got that far.
func (d *Dir) Capture() (*Capture, error) {
	return readCapture(filepath.Join(d.path, captureFile))
}

// Calls returns the shim's journal: every docker call it saw, in order.
func (d *Dir) Calls() ([]Call, error) {
	return readCalls(filepath.Join(d.path, callsFile))
}

// Remove deletes the directory and everything in it.
func (d *Dir) Remove() error {
	return os.RemoveAll(d.path)
}
