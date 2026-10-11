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

// HalfCloseDialer is a PortDialer whose connections may end soon after the
// daemon's side half-closes them (Kubernetes: the cluster's runtime closes a
// port-forward about a second after it reads the local EOF, so a peer that
// answers later loses its answer). DialPortHalfClose opens a connection that
// keeps a true half-close (Kubernetes: through the pod agent), for a forward
// that asks for one (`fleet port-forward add --half-close`). Backends whose
// connections already keep it have no need of it.
type HalfCloseDialer interface {
	DialPortHalfClose(ctx context.Context, containerID string, port int) (io.ReadWriteCloser, error)
}

// PortWarmer is a PortDialer that can set up what its dials share ahead of
// the first one (Kubernetes: the pod's port-forward connection), so the first
// connection of a burst, a browser's first page load, does not wait for it.
type PortWarmer interface {
	WarmPorts(ctx context.Context, containerID string) error
}

// PortPublisher is a backend whose instances' published ports (`docker run
// -p`, a devcontainer's appPort) the daemon serves itself, as listeners on the
// daemon host that dial into the instance (PortDialer): Kubernetes. Docker
// publishes its containers' ports on its own.
type PortPublisher interface {
	// PublishedPorts are the instance's published ports, as its create
	// recorded them.
	PublishedPorts(containerID string) ([]PublishedPort, error)
}

// PublishedPort is one published port: a listener on HostIP:HostPort
// (HostPort 0 leaves the port to the daemon) forwarding to ContainerPort in
// the instance.
type PublishedPort struct {
	HostIP        string `json:"hostIP"`
	HostPort      int    `json:"hostPort,omitempty"`
	ContainerPort int    `json:"containerPort"`
}
