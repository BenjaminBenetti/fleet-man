// Package hostrelay is the daemon host's side of what a Kubernetes pod reaches
// on it, the two docker features a pod has no bind mount or bridge for:
//
//   - The unix sockets a devcontainer binds from the daemon host (a gpg
//     agent's, an X11 display's, Wayland's, pulse's): the pod's agent binds a
//     socket at each bind's target, and fleetd dials the daemon-host path it
//     recorded at create (Socket, Dial) for every connection relayed to it.
//   - host.docker.internal / host-gateway: the pod's agent mirrors the
//     daemon host's listening TCP ports (Scanner) on 127.0.0.2, and fleetd
//     dials the port where it listens for every connection relayed to it.
//
// A pod only ever names what fleetd recorded or found: a recorded socket's
// target, a port in the listening set. What is dialled is fleetd's own record.
// The package is stdlib-only (plus x/sys/unix) and builds on every OS fleet
// ships for.
package hostrelay

import (
	"io"
	"io/fs"
	"net"
	"path/filepath"
)

// Socket is one daemon-host unix socket a devcontainer binds, as fleetd
// records it at create: the pod's agent binds Target, and each connection to
// it is relayed to Source.
type Socket struct {
	// Target is the socket's path in the dev container.
	Target string `json:"target"`
	// Source is the socket's path on the daemon host, resolved (Resolve) when
	// it was recorded.
	Source string `json:"source"`
	// Mode is the daemon-host socket's permission bits, which the pod's
	// socket gets too (its owner is the devcontainer's user).
	Mode fs.FileMode `json:"mode,omitempty"`
}

// Resolve is path as a recorded Source holds it: absolute, every symbolic
// link on the way resolved, so Dial can refuse a link that appears there
// later.
func Resolve(path string) (string, error) {
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	return filepath.Abs(real)
}

// Splice relays a and b until both directions end: the end of one side's
// input is a half-close of the other's output (CloseWrite, where the
// connection has one), so a request answered after its sender's EOF still
// arrives. An error in either direction ends both.
func Splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	copyTo := func(dst, src net.Conn) {
		if _, err := io.Copy(dst, src); err == nil {
			closeWrite(dst)
		} else {
			_ = a.Close()
			_ = b.Close()
		}
		done <- struct{}{}
	}
	go copyTo(a, b)
	go copyTo(b, a)
	<-done
	<-done
	_ = a.Close()
	_ = b.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
