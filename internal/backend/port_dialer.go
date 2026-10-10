package backend

import (
	"context"
	"io"
)

// PortDialer is an optional capability for backends that open a TCP
// connection to a port inside an instance from the daemon process itself:
// Kubernetes takes one stream pair on a pooled port-forward connection to the
// pod. The server's Forward data plane tries it before anything else, and for
// a backend that has it uses no other way in: no direct dial
// (ResolveHostname) and no process per connection (ForwardStdioCommand,
// PortForwardCommand).
type PortDialer interface {
	// DialPort connects to port on the instance's loopback. The connection may
	// also implement CloseWrite() error, a half-close: the peer reads EOF and
	// can still answer. Closing the connection ends it in both directions.
	DialPort(ctx context.Context, containerID string, port int) (io.ReadWriteCloser, error)
}
