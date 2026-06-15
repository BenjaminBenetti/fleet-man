package fleetclient

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// copyfile.go holds the host CLI's local-path policy for `fleet copy`: a process
// run directly on a machine resolves local paths relative to its own cwd. The
// in-instance form's policy (the human's home / downloads folder) lives in the
// TUI, which runs the delegated copy. Both implement CopyLocalPolicy so the one
// engine in copyengine.go serves both.

// HostLocalPolicy resolves local paths for a `fleet` process run directly on a
// machine: a source is read as given (relative to the cwd), and a destination
// follows scp's rule that an empty or directory dest keeps the source basename.
type HostLocalPolicy struct{}

// ResolveSrc returns the source path unchanged — the os layer resolves a
// relative path against the process cwd, which is what the user means.
func (HostLocalPolicy) ResolveSrc(path string) string { return path }

// ResolveDest applies ResolveCopyDest (cwd-relative, scp dest semantics).
func (HostLocalPolicy) ResolveDest(dest, srcName string) (string, error) {
	return ResolveCopyDest(dest, srcName)
}

// ResolveCopyDest resolves the local destination path for a copied file named
// name: an empty dest or a dest that is (or is spelled like) a directory keeps
// the source basename inside it.
func ResolveCopyDest(dest, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("server sent an empty file name")
	}
	if dest == "" {
		return name, nil
	}
	if strings.HasSuffix(dest, string(os.PathSeparator)) || strings.HasSuffix(dest, "/") {
		return filepath.Join(dest, name), nil
	}
	if fi, err := os.Stat(dest); err == nil && fi.IsDir() {
		return filepath.Join(dest, name), nil
	}
	return dest, nil
}
