//go:build nok8s

package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// IssueKubeExecCredential: the pod build of fleet has no Kubernetes backend.
func (s *service) IssueKubeExecCredential(context.Context, *fleetgrpc.IssueKubeExecCredentialRequest) (*fleetgrpc.IssueKubeExecCredentialReply, error) {
	return nil, status.Error(codes.Unimplemented, "this fleet build has no Kubernetes support")
}
