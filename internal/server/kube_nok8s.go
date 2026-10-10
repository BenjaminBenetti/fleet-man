//go:build nok8s

package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_nok8s.go is the Kubernetes side of a -tags nok8s build (the in-pod
// helper), which links no client-go: there are no connections, and the kube
// RPCs answer Unimplemented through the embedded
// UnimplementedFleetServiceServer.

type kubeManager struct{}

func newKubeManager(*service) *kubeManager { return &kubeManager{} }

func (*kubeManager) run(ctx context.Context) { <-ctx.Done() }

func (*kubeManager) deployTargets(bool) []fleet.DeployTargetOption { return nil }

func (*kubeManager) defaultCluster(string) string { return "" }

func (*kubeManager) createAllowed(*state.State, fleet.DeployTarget) error { return nil }
