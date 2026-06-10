// Package fleetcopy is the in-instance half of `fleet copy` (alias fc): it
// asks the HOST fleet to copy a file out of this instance onto the user's
// machine. Like the `fleet launch` TUI (internal/launchtui), the in-container
// process cannot reach the user's disk itself, so it writes a file.copy
// envelope to the control socket fleet bind-mounts into the instance; the
// server turns it into a Watch FileCopy event and the connected fleet TUI
// pulls the bytes over the CopyFile RPC into its local downloads folder.
package fleetcopy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
)

// Config parameterises Request. The zero value is the normal in-instance case:
// dial the standard control socket.
type Config struct {
	// SocketPath overrides the control socket the client dials. Empty means the
	// standard container-side path (control.ContainerSocketPath). Tests set
	// this to a temp socket.
	SocketPath string
}

// socketPath resolves the control socket to dial.
func (c Config) socketPath() string {
	if c.SocketPath != "" {
		return c.SocketPath
	}
	return control.ContainerSocketPath
}

// InInstance reports whether this process runs inside a fleet instance, by the
// presence of the control mount directory fleet bind-mounts into every
// instance. The directory — not the socket file — is the right probe: the
// socket only exists while a host TUI is attached, but the mount is there for
// the instance's whole life, so command help can pick the in-instance form
// even when no host is connected yet.
func InInstance() bool {
	fi, err := os.Stat(control.ContainerMountDir)
	return err == nil && fi.IsDir()
}

// Request resolves the file locally (fast feedback on typos — the send itself
// is fire-and-forget) and asks the host fleet to copy it out via the control
// socket, exactly like `fleet launch` asks for a browser open. dest is the
// requested destination on the user's machine, passed through verbatim ("" =
// the user's downloads folder) — only the receiving client can interpret a
// path on its own filesystem.
func Request(cfg Config, out io.Writer, path, dest string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("%s is a directory — only single files can be copied", path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}

	client, err := control.Dial(cfg.socketPath())
	if err != nil {
		return fmt.Errorf("not connected to a host fleet — copying out of an instance needs a running fleet TUI on the host")
	}
	defer client.Close()
	if err := client.CopyFile(abs, dest); err != nil {
		return err
	}
	if dest != "" {
		fmt.Fprintf(out, "Asked the host fleet to copy %s — it will land at %s on your machine.\n", abs, dest)
	} else {
		fmt.Fprintf(out, "Asked the host fleet to copy %s — it will land in your downloads folder.\n", abs)
	}
	return nil
}
