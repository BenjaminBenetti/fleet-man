//go:build !nok8s

package server

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
)

// issueKubeExecCredential issues a connection's transport; a var so tests
// stand in for the daemon's pool.
var issueKubeExecCredential = func(ctx context.Context, clusterID string, refresh bool) (fleet.KubeExecCredential, error) {
	return kubernetes.DefaultPool().IssueExecCredential(ctx, clusterID, refresh)
}

// IssueKubeExecCredential hands the daemon-host `fleet kube-exec` shim the
// transport fleetd reaches a connection's API server with, from fleetd's own
// credential cache, so a direct-mode TTY attach never runs the connection's
// credential plugin itself.
//
// It is local-socket only (localOnlyMethods): the shim runs on the daemon
// host, and a remote client (gateway, Armada SSH) reaches the very same pods,
// shells and panes through the streaming Exec RPC, which fleetd serves over
// its own connection; that is how the remote full-control rule holds without
// a cluster credential ever leaving the daemon host.
func (s *service) IssueKubeExecCredential(ctx context.Context, req *fleetgrpc.IssueKubeExecCredentialRequest) (*fleetgrpc.IssueKubeExecCredentialReply, error) {
	if req.GetClusterId() == "" {
		return nil, status.Error(codes.InvalidArgument, "no Kubernetes connection named")
	}
	cred, err := issueKubeExecCredential(ctx, req.GetClusterId(), req.GetRefresh())
	if err != nil {
		return nil, status.Error(kubeCredentialCode(err), err.Error())
	}
	return &fleetgrpc.IssueKubeExecCredentialReply{Credential: protoconv.KubeExecCredentialToProto(cred)}, nil
}

// kubeCredentialCode is the gRPC code of an issue that failed. A plugin's
// failure (*kubeexec.PluginError) carries only its sanitized stderr.
func kubeCredentialCode(err error) codes.Code {
	var plugin *kubeexec.PluginError
	switch {
	case errors.Is(err, kubernetes.ErrClusterNotFound):
		return codes.NotFound
	case errors.Is(err, context.Canceled):
		return codes.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return codes.DeadlineExceeded
	case errors.As(err, &plugin):
		return codes.Unauthenticated
	case errors.Is(err, kubeexec.ErrKubeconfigMissing), errors.Is(err, kubeexec.ErrContextMissing),
		errors.Is(err, kubeexec.ErrPluginMissing), errors.Is(err, kubeexec.ErrMisconfigured):
		return codes.FailedPrecondition
	}
	return codes.Unavailable
}
