//go:build nok8s

package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kubePods has nothing to do in a -tags nok8s build: there are no
// Kubernetes instances.
type kubePods struct{}

func newKubePods(*service) *kubePods { return &kubePods{} }

func (*kubePods) run(ctx context.Context) { <-ctx.Done() }

func (*kubePods) onState(*state.State) {}

func (*kubePods) attached(string, string) {}

func (*kubePods) started(string, string) {}
