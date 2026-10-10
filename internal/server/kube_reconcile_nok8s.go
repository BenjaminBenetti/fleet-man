//go:build nok8s

package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kubeRecovery has nothing to reconcile in a -tags nok8s build: there are no
// Kubernetes jobs.
type kubeRecovery struct{}

func newKubeRecovery(*service) *kubeRecovery { return &kubeRecovery{} }

func (*kubeRecovery) load() {}

func (*kubeRecovery) claim(*state.State) {}

func (*kubeRecovery) claims(string, string) bool { return false }

func (*kubeRecovery) interruptedError(string, *fleet.Instance) string { return interruptedByRestart }

func (*kubeRecovery) settle() {}

func (*kubeRecovery) start(context.Context) {}

func (*kubeRecovery) destroying(string, *fleet.Instance) {}

func (*kubeRecovery) destroyed(*fleet.Instance) {}
