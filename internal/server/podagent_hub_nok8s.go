//go:build nok8s

package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// podAgentHub has nothing to do in a -tags nok8s build: there are no
// Kubernetes pods.
type podAgentHub struct{}

func newPodAgentHub(*service) *podAgentHub { return &podAgentHub{} }

func (*podAgentHub) run(ctx context.Context) { <-ctx.Done() }

func (*podAgentHub) onState(*state.State) {}
