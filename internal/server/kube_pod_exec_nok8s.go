//go:build nok8s

package server

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
)

// PodExec: the pod build of fleet has no Kubernetes backend.
func (s *service) PodExec(fleetgrpc.FleetService_PodExecServer) error {
	return status.Error(codes.Unimplemented, "this fleet build has no Kubernetes support")
}
