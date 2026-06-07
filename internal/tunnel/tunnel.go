// Package tunnel defines the wire protocol shared by the two ends of the
// remote-MCP reverse tunnel: the fleetd side (internal/server/remote), which
// dials OUT to a gateway and serves inbound MCP requests, and the fleet gateway
// (internal/gateway, a later PR), which accepts those dials and routes public
// MCP traffic back down them.
//
// It is a small leaf package — it imports only the standard library and yamux —
// so both server-side modules can share the exact frame definitions without
// coupling to each other or to fleet internals.
//
// # Connection lifecycle
//
//  1. fleetd dials the gateway (TLS) and writes ONE RegisterRequest control
//     frame on the raw conn; the gateway replies with ONE RegisterReply frame.
//     This length-prefixed JSON handshake happens BEFORE yamux is layered on, so
//     registration stays a simple typed exchange.
//  2. Both sides then wrap the SAME conn in yamux (fleetd as client, gateway as
//     server). The gateway opens one yamux stream per inbound MCP request; fleetd
//     accepts each stream and serves it as a normal HTTP connection proxied to
//     its loopback MCP server. One stream per request means SSE streams and
//     concurrent calls never interleave.
//
// There is no authentication frame: the gateway authorizes nobody. The
// unguessable public URL it mints per session is the capability that isolates
// each fleetd, and the MCP bearer token (carried end to end in the Authorization
// header) remains the real access boundary on the loopback server.
package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"

	"github.com/hashicorp/yamux"
)

// RegisterRequest is the first frame fleetd writes after dialing the gateway.
// SessionID is empty on a first registration and set to the previously-assigned
// id on reconnect, so the gateway can hand back the SAME public URL (sticky).
type RegisterRequest struct {
	// SessionID, when non-empty, asks the gateway to re-use a prior session's
	// public URL (sticky reconnect). Empty requests a fresh one.
	SessionID string `json:"session_id,omitempty"`
	// ClientVersion is the fleetd version, for the gateway's logs/diagnostics.
	ClientVersion string `json:"client_version,omitempty"`
}

// RegisterReply is the gateway's response. On success Error is empty, SessionID
// is the assigned (possibly reused) id, and PublicURL is the address external
// MCP clients use. On failure Error explains why and the other fields are empty.
type RegisterReply struct {
	SessionID string `json:"session_id,omitempty"`
	PublicURL string `json:"public_url,omitempty"`
	Error     string `json:"error,omitempty"`
}

// MaxFrameSize caps a control frame so a malicious or garbled peer cannot make
// the reader allocate unbounded memory. Control frames are tiny JSON objects.
const MaxFrameSize = 64 << 10

// ErrFrameTooLarge is returned when a frame exceeds MaxFrameSize.
var ErrFrameTooLarge = errors.New("tunnel: control frame too large")

// WriteFrame writes v as a length-prefixed JSON control frame: a 4-byte
// big-endian length followed by the JSON bytes.
func WriteFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadFrame reads a length-prefixed JSON control frame (written by WriteFrame)
// into v. It rejects oversized length prefixes before allocating.
func ReadFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameSize {
		return ErrFrameTooLarge
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// sessionConfig returns the shared yamux config. yamux keepalive (enabled by
// default) keeps NAT mappings warm and detects a dead peer at the frame layer,
// so neither side needs an application-level heartbeat. logOutput receives
// yamux's internal logging; pass io.Discard to silence it.
func sessionConfig(logOutput io.Writer) *yamux.Config {
	cfg := yamux.DefaultConfig()
	if logOutput != nil {
		cfg.LogOutput = logOutput
	}
	return cfg
}

// ClientSession wraps the dialing side (fleetd) of an established control conn,
// after the RegisterRequest/RegisterReply handshake has completed on it.
func ClientSession(conn net.Conn, logOutput io.Writer) (*yamux.Session, error) {
	return yamux.Client(conn, sessionConfig(logOutput))
}

// ServerSession wraps the accepting side (the gateway) of an established control
// conn, after the handshake has completed on it.
func ServerSession(conn net.Conn, logOutput io.Writer) (*yamux.Session, error) {
	return yamux.Server(conn, sessionConfig(logOutput))
}
