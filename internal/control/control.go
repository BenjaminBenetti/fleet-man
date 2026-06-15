// Package control is a generic host↔instance IPC channel built on a unix
// domain socket that fleet bind-mounts into an instance.
//
// The split is deliberate: fleet's browser lives on the host, while features
// like the `fleet launch` TUI run inside an instance. Those in-container
// processes can't drive the host browser directly, so they speak to the host
// over this socket. The host runs a Server (the listener that owns the socket
// file); the instance connects as a Client and writes messages.
//
// The wire format is newline-delimited JSON — one Envelope per line. The
// Envelope is a thin, type-discriminated container: a Type string plus an
// opaque JSON Payload that only the handler registered for that Type knows
// how to decode. New features add a new Type constant and a new payload
// struct without touching the transport, so the channel stays generic.
//
// A unix socket created by the host inside a bind-mounted directory is
// reachable from inside the container (same kernel, shared mount) — the same
// mechanism fleet already relies on to share docker.sock. The socket basename
// (SocketName) MUST be identical on both ends because the host derives the
// host-side absolute path while the instance uses the container-side mount
// path, and both must resolve to the same file through the bind mount.
package control

// Path constants for the container side of the channel. The host side derives
// its own absolute path via internal/state; both ends resolve to the SAME
// file through the bind mount, so the socket basename (SocketName) MUST be
// identical on both ends.
const (
	// ContainerMountDir is the directory the host control directory is
	// bind-mounted to inside the instance.
	ContainerMountDir = "/fleet-mounts/control"
	// SocketName is the socket file's basename. It is shared by the host and
	// the container ends so both resolve to the same file through the mount.
	SocketName = "fleet.sock"
	// ContainerSocketPath is the absolute path the instance dials: the mount
	// directory joined with the shared socket basename.
	ContainerSocketPath = ContainerMountDir + "/" + SocketName
)

// Message type identifiers. Each value names a distinct kind of Envelope; the
// handler switches on it to pick the matching payload struct. New features add
// a new constant here alongside a new payload type.
const (
	// TypeOpenBrowser asks the host to open (or navigate) its browser to a
	// URL. Its payload is OpenBrowserPayload.
	TypeOpenBrowser = "browser.open"
	// TypeCopyFile asks the connected host TUI to perform a scp-style copy
	// between two endpoints (out of, into, or between instances) on this
	// instance's behalf. Its payload is CopyFilePayload.
	TypeCopyFile = "file.copy"
)
