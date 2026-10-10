//go:build !nok8s

package server

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeexec"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
)

// issueKubeExecCredential issues a connection's transport; a var so tests
// stand in for the daemon's pool.
var issueKubeExecCredential = func(ctx context.Context, clusterID string, refresh bool) (fleet.KubeExecCredential, error) {
	return kubernetes.DefaultPool().IssueExecCredential(ctx, clusterID, refresh)
}

// verifyKubeExecPod verifies the pod a direct-mode attach is about to reach
// and returns its uid: it must be the pod of one of this daemon's instances on
// the connection, and pass the check the pool makes before it gives a pod an
// agent session (its image, its spec, its controller). A var so tests stand
// in for the pool.
var verifyKubeExecPod = func(ctx context.Context, clusterID, pod string) (string, error) {
	inst, err := resolveKubePod(clusterID, pod)
	if err != nil {
		return "", err
	}
	tp, ok := backendutil.NewForInstance(inst, false).(kubernetes.TargetProvider)
	if !ok {
		return "", status.Errorf(codes.FailedPrecondition, "instance %s has no pod", inst.Name)
	}
	t, err := tp.PodTarget()
	if err != nil {
		return "", status.Errorf(codes.FailedPrecondition, "the pod of instance %s: %v", inst.Name, err)
	}
	p, err := kubernetes.DefaultPool().Verify(ctx, t)
	if err != nil {
		return "", status.Error(kubePodVerifyCode(err), err.Error())
	}
	return string(p.UID), nil
}

// kubePodVerifyCode is the gRPC code of a pod that did not verify:
// PermissionDenied for a substituted one (the shim gives up on it),
// FailedPrecondition for one that cannot be checked now (not running yet,
// the API server away; the shim tries again). Never Unavailable: the shim
// reads that as fleetd being away and goes to the kubeconfig instead.
func kubePodVerifyCode(err error) codes.Code {
	var tampered *kubernetes.TamperedError
	switch {
	case errors.As(err, &tampered):
		return codes.PermissionDenied
	case errors.Is(err, context.Canceled):
		return codes.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return codes.DeadlineExceeded
	}
	return codes.FailedPrecondition
}

// IssueKubeExecCredential hands the daemon-host `fleet kube-exec` shim the
// transport fleetd reaches a connection's API server with, from fleetd's own
// credential cache, so a direct-mode TTY attach never runs the connection's
// credential plugin itself. A request that names the pod it attaches to gets
// the credential only once that pod verifies, and its uid with it (the shim
// binds the attach to it).
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
	var podUID string
	if pod := req.GetPod(); pod != "" {
		uid, err := verifyKubeExecPod(ctx, req.GetClusterId(), pod)
		if err != nil {
			return nil, err
		}
		podUID = uid
	}
	cred, err := issueKubeExecCredential(ctx, req.GetClusterId(), req.GetRefresh())
	if err != nil {
		return nil, status.Error(kubeCredentialCode(err), err.Error())
	}
	return &fleetgrpc.IssueKubeExecCredentialReply{Credential: protoconv.KubeExecCredentialToProto(cred), PodUid: podUID}, nil
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
