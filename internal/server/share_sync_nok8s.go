//go:build nok8s

package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// shareSync has nothing to do in a -tags nok8s build: there are no cluster
// shares to sync, and the sync RPCs answer Unimplemented through the
// embedded UnimplementedFleetServiceServer.
type shareSync struct{}

// fleetFlush is a fleet destroy's flush: nothing to flush.
type fleetFlush struct{}

func newShareSync(*service) *shareSync { return &shareSync{} }

func (*shareSync) run(ctx context.Context) { <-ctx.Done() }

func (*shareSync) onState(*state.State) {}

func (*shareSync) ready(context.Context, string, string) error { return nil }

func (*shareSync) beforeStop(string, string) {}

func (*shareSync) beforeDestroy(string, string, bool) fleetFlush { return fleetFlush{} }

func (*shareSync) retireFleet(string, fleetFlush) []string { return nil }

func (*shareSync) flushAndClose(context.Context) error { return nil }

func (*shareSync) kickCluster(string, string) {}
