//go:build !nok8s

package kubeexec

import (
	"context"
	"io"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// transport.go is an opaque handle on a cluster client configuration, so a
// client package (the daemon-host `fleet kube-exec` shim in internal/cli) can
// drive execs, logs and streams through kubeexec without ever naming a
// k8s.io/client-go type itself — the import boundary keeps client-go to this
// one leaf.

// Transport is the client configuration an attach reaches a pod with: fleetd's
// issued credential (TransportFor) or the kubeconfig a Connection loaded
// (Connection.Transport).
type Transport struct{ cfg *restConfig }

// restConfig is k8s.io/client-go/rest.Config, aliased so this file names it
// once (the rest of the package already imports rest).
type restConfig = restConfigType

// TransportFor builds a Transport from fleetd's issued credential.
func TransportFor(cred fleet.KubeExecCredential) (*Transport, error) {
	cfg, err := RESTConfigFor(cred)
	if err != nil {
		return nil, err
	}
	return &Transport{cfg: cfg}, nil
}

// Transport is the connection's own client configuration (the kubeconfig it
// loaded), for when fleetd is not available to issue one.
func (c *Connection) Transport() *Transport { return &Transport{cfg: c.RESTConfig()} }

// Stream starts a kube exec of req's command in the pod and returns its stdio
// as one byte stream (StartStream over this transport's configuration).
func (t *Transport) Stream(ctx context.Context, req ExecRequest) *Stream {
	cfg := t.cfg
	return StartStream(ctx, req, func(ctx context.Context, req ExecRequest, stdio Stdio) error {
		return ExecConfig(ctx, cfg, req, stdio)
	})
}

// Exec runs req to completion over this transport, bridging stdio (the raw
// fallback for a pod with no fleet binary).
func (t *Transport) Exec(ctx context.Context, req ExecRequest, stdio Stdio) error {
	return ExecConfig(ctx, t.cfg, req, stdio)
}

// PodLogs streams a pod container's log over this transport.
func (t *Transport) PodLogs(ctx context.Context, req PodLogsRequest, w io.Writer) error {
	return PodLogs(ctx, t.cfg, req, w)
}
