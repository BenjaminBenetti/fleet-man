// Package fleetcopy is the in-instance half of scp-style `fleet copy` (alias
// fc): it asks the connected HOST fleet TUI to perform a copy between two
// endpoints on the in-container caller's behalf. Like the `fleet launch` TUI
// (internal/launchtui), the in-container process cannot reach the host fleetd
// over gRPC — only the control socket fleet bind-mounts into the instance — so
// it writes a file.copy envelope naming the two endpoints; the server turns it
// into a Watch FileCopy event and the connected TUI runs the generic copy engine
// against the host fleet (with its own disk as the "local" side). This is what
// lets `fc` reach the user's machine even when the instance is fully remote, and
// lets it copy between sibling instances of the user's fleet.
package fleetcopy

import (
	"fmt"
	"io"
	"os"

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

// Request asks the connected host fleet TUI to copy from src to dst (endpoints
// as typed inside the instance), passed through verbatim over the control
// socket. It is a pure signal — fire-and-forget, like `fleet launch` asking for
// a browser open: the in-container process can resolve neither the user's-machine
// paths nor the host fleet's instances, so it does not validate the endpoints;
// the TUI performs the copy and surfaces any error there. An empty dst is the
// download shorthand (deliver to the user's downloads folder).
func Request(cfg Config, out io.Writer, src, dst string) error {
	client, err := control.Dial(cfg.socketPath())
	if err != nil {
		return fmt.Errorf("not connected to a host fleet — `fc` needs a running fleet TUI on the host")
	}
	defer client.Close()
	if err := client.CopyFile(src, dst); err != nil {
		return err
	}
	if dst != "" {
		fmt.Fprintf(out, "Asked the host fleet to copy %s -> %s.\n", src, dst)
	} else {
		fmt.Fprintf(out, "Asked the host fleet to copy %s — it will land in your downloads folder.\n", src)
	}
	return nil
}
