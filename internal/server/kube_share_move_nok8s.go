//go:build nok8s

package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kubeShareMoves has nothing to move in a -tags nok8s build: there are no
// cluster shares.
type kubeShareMoves struct{}

func newKubeShareMoves(*service) *kubeShareMoves { return &kubeShareMoves{} }

func (*kubeShareMoves) run(ctx context.Context) { <-ctx.Done() }

func (*kubeShareMoves) onState(*state.State) {}

func (*kubeShareMoves) poke() {}
