package tunnel

import "io"

// tag.go defines the per-stream type tag used once the FeatureGRPC capability is
// negotiated. Every yamux stream the gateway opens then begins with a single TAG
// byte that tells fleetd how to handle the rest of the stream:
//
//	TagMCP     → the stream carries an HTTP request for the loopback MCP server.
//	TagGRPC    → the stream carries a raw native-gRPC (HTTP/2) connection that fleetd
//	             splices to its gRPC server.
//	TagWebhook → the stream carries an HTTP request for the automation webhook
//	             receiver (an inbound POST the gateway accepted at
//	             <public-url>/webhook/<name>).
//
// The gateway writes the tag as the FIRST bytes of the stream, before relaying
// any client/payload bytes, so fleetd can read it immediately without sniffing
// or blocking on data that has not arrived yet. The tag is a stream-level prefix,
// NOT a control frame — it rides inside an established yamux session, after the
// RegisterRequest/RegisterReply handshake.
//
// Because an un-upgraded peer does not know about tags, tags are used ONLY when
// a tagging feature (FeatureGRPC or FeatureWebhook) is in the negotiated feature
// set (see tunnel.go); otherwise the tunnel stays byte-for-byte the legacy
// MCP-only stream.
const (
	TagMCP     byte = 0x00
	TagGRPC    byte = 0x01
	TagWebhook byte = 0x02
)

// WriteTag writes a single stream-type tag byte.
func WriteTag(w io.Writer, tag byte) error {
	_, err := w.Write([]byte{tag})
	return err
}

// ReadTag reads a single stream-type tag byte.
func ReadTag(r io.Reader) (byte, error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}
