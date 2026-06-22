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
//  1. fleetd opens a gRPC bidi Register stream on the gateway's gRPC endpoint and
//     wraps it as a net.Conn (StreamConn). It writes ONE RegisterRequest control
//     frame; the gateway replies with ONE RegisterReply frame. This length-prefixed
//     JSON handshake happens BEFORE yamux is layered on, so registration stays a
//     simple typed exchange. (There is no dedicated TCP control port — the same
//     handshake/yamux code runs over the stream-as-net.Conn.)
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
	"slices"

	"github.com/hashicorp/yamux"
)

// RegisterRequest is the first frame fleetd writes after dialing the gateway.
// SessionID is empty on a first registration and set to the previously-assigned
// id on reconnect, so the gateway can hand back the SAME public URL (sticky).
type RegisterRequest struct {
	// SessionID, when non-empty, asks the gateway to re-use a prior session's
	// public URL (sticky reconnect). Empty requests a fresh one.
	SessionID string `json:"session_id,omitempty"`
	// SessionToken, when non-empty, is the gateway-signed session token from a
	// previous RegisterReply. A gateway that does not recognize SessionID
	// (typically because it restarted and lost its in-memory registry) verifies
	// the token's signature against its session key and, on success, resurrects
	// the session under its ORIGINAL public URL instead of minting a fresh one.
	SessionToken string `json:"session_token,omitempty"`
	// ClientVersion is the fleetd version, for the gateway's logs/diagnostics.
	ClientVersion string `json:"client_version,omitempty"`
	// Features lists optional tunnel capabilities fleetd supports (e.g.
	// FeatureGRPC). The gateway echoes back the subset it ALSO supports in
	// RegisterReply.Features; both sides then only use a feature when it appears
	// in that negotiated set. Absent (old fleetd) means the base MCP-only tunnel.
	Features []string `json:"features,omitempty"`
}

// RegisterReply is the gateway's response. On success Error is empty, SessionID
// is the assigned (possibly reused) id, and PublicURL is the address external
// MCP clients use. On failure Error explains why and the other fields are empty.
type RegisterReply struct {
	SessionID string `json:"session_id,omitempty"`
	PublicURL string `json:"public_url,omitempty"`
	// PublicGRPCURL is the address a remote `fleet` binary dials to control this
	// daemon through the gateway (the "Public GRPC URL"). Only set when the grpc
	// feature is negotiated AND the gateway is configured with a public gRPC base
	// URL (--public-grpc-url); empty otherwise (incl. from an old gateway).
	PublicGRPCURL string `json:"public_grpc_url,omitempty"`
	// PublicWebhookURL is the gateway-assigned base URL for this daemon's
	// automation webhooks (<public-url>/webhook/<id>). A webhook trigger's full
	// URL is this base + "/" + the trigger's webhook name. Only set when the
	// webhook feature is negotiated; empty otherwise (incl. from an old gateway).
	// Unlike PublicGRPCURL it rides the SAME public base as the MCP URL (the
	// gateway serves webhooks on its public HTTP listener), so no separate
	// gateway base URL is needed.
	PublicWebhookURL string `json:"public_webhook_url,omitempty"`
	// SessionToken is a JWT, signed with the gateway's session key, encoding
	// this session's ids. fleetd persists it with the session id and presents
	// both on reconnect, so the session (and its public URL) survives a gateway
	// restart when the gateway runs with a stable --session-key. Empty from an
	// old gateway.
	SessionToken string `json:"session_token,omitempty"`
	Error        string `json:"error,omitempty"`
	// Features is the negotiated set: the intersection of what fleetd requested
	// and what the gateway supports. Absent (old gateway) means MCP-only.
	Features []string `json:"features,omitempty"`
	// GatewayVersion is the gateway's build version, for diagnostics: fleetd
	// surfaces it to remote TUIs (over RemoteMcpStatus) so the whole control
	// chain's versions are visible. Empty from an old gateway predating the field.
	GatewayVersion string `json:"gateway_version,omitempty"`
}

// FeatureGRPC negotiates tunneling the daemon's gRPC server alongside MCP. When
// it is in the negotiated set, the gateway tags each opened stream (see tag.go)
// and serves the gRPC proxy route, and fleetd demuxes tagged streams.
const FeatureGRPC = "grpc"

// FeatureWebhook negotiates tunneling the daemon's automation webhook receiver
// alongside MCP. When it is in the negotiated set, the gateway tags each opened
// stream (see tag.go), serves the public /webhook/<id>/<name> route, and mints a
// PublicWebhookURL; fleetd demuxes TagWebhook streams to its webhook receiver.
// Like FeatureGRPC it forces the tagged-stream wire, so either feature alone is
// enough to switch the tunnel into demux mode.
const FeatureWebhook = "webhook"

// RegisterMethod is the gRPC full-method fleetd opens (a bidi stream) on the
// gateway's gRPC port to register and carry the reverse tunnel. The gateway's
// grpc.Server registers a matching service (ServiceName "fleet.gateway.Tunnel",
// stream "Register"); both ends must agree on this string.
const RegisterMethod = "/fleet.gateway.Tunnel/Register"

// HasFeature reports whether features contains f.
func HasFeature(features []string, f string) bool {
	return slices.Contains(features, f)
}

// Negotiate returns the features present in BOTH requested and supported (the
// set the gateway puts in RegisterReply.Features), preserving requested order.
func Negotiate(requested, supported []string) []string {
	var out []string
	for _, r := range requested {
		if HasFeature(supported, r) {
			out = append(out, r)
		}
	}
	return out
}

// MaxFrameSize caps a control frame so a malicious or garbled peer cannot make
// the reader allocate unbounded memory. Control frames are tiny JSON objects.
const MaxFrameSize = 64 << 10

// ErrFrameTooLarge is returned when a frame exceeds MaxFrameSize.
var ErrFrameTooLarge = errors.New("tunnel: control frame too large")

// ErrHandshakeTimeout is returned by Handshake / ReadRegisterRequest when the
// register exchange does not complete within the deadline.
var ErrHandshakeTimeout = errors.New("tunnel: register handshake timed out")

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
