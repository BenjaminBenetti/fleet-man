//go:build nok8s

package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	kubecheck "github.com/BenjaminBenetti/fleet-man/internal/inspector/check/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/server/remote"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_nok8s.go is the Kubernetes side of a -tags nok8s build (the in-pod
// helper), which links no client-go: there are no connections, and the kube
// RPCs answer Unimplemented through the embedded
// UnimplementedFleetServiceServer.

type kubeManager struct{}

func newKubeManager(*service) *kubeManager { return &kubeManager{} }

func (*kubeManager) run(ctx context.Context) { <-ctx.Done() }

// No connections, so no image delivery and no gateway image server.
func (*kubeManager) startImages(context.Context, bool) []remote.Option { return nil }

func (*kubeManager) attachTunnel(*remote.Manager) {}

func (*kubeManager) deployTargets(kubecheck.Config, bool) []fleet.DeployTargetOption { return nil }

func (*kubeManager) defaultCluster(string) string { return "" }

func (*kubeManager) connected(string) bool { return false }

func (*kubeManager) createAllowed(*state.State, fleet.DeployTarget) error { return nil }

func (*kubeManager) quotaRoom(*state.State, string, int) error { return nil }

func (*kubeManager) automationLimits(string) (clusterLimits, bool) { return clusterLimits{}, false }
