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

import "encoding/json"

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
)

// Envelope is the generic wire message: a type discriminator plus an opaque
// JSON payload. The transport carries the Envelope verbatim; the handler
// registered for env.Type decodes env.Payload into the matching payload
// struct. Keeping the payload as json.RawMessage means the transport never
// needs to know about individual message shapes.
type Envelope struct {
	// Type names the kind of message — one of the Type* constants. The
	// handler switches on it to choose how to decode Payload.
	Type string `json:"type"`
	// Payload is the message body, left undecoded by the transport. It is
	// omitted from the wire when empty so type-only messages stay compact.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// OpenBrowserPayload is the body of a TypeOpenBrowser Envelope: the address
// the host browser should open or navigate to.
type OpenBrowserPayload struct {
	// URL is the address to open. The host opens its proxied browser to this
	// URL (or forwards it as a new tab to an already-running browser).
	URL string `json:"url"`
}
