package control

import "encoding/json"

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
