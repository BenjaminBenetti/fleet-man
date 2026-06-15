package cli

import (
	"context"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
)

// TestRebuildCmdRunsJob verifies `fleet rebuild <name>` resolves its target and
// drives the lifecycle job runner (the gRPC RebuildInstance path is exercised by
// the server tests).
func TestRebuildCmdRunsJob(t *testing.T) {
	orig := runInstanceJob
	defer func() { runInstanceJob = orig }()

	called := false
	runInstanceJob = func(ctx context.Context, open func(context.Context, fleetgrpc.FleetServiceClient) (grpc.ServerStreamingClient[fleetgrpc.JobEvent], error)) error {
		called = true
		return nil
	}

	cmd := newRebuildCmd()
	cmd.SetArgs([]string{"alpha/i1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rebuild execute: %v", err)
	}
	if !called {
		t.Fatal("runInstanceJob was not invoked")
	}
}

// TestRebuildCmdAlias locks in the short `rb` alias.
func TestRebuildCmdAlias(t *testing.T) {
	got := newRebuildCmd().Aliases
	if len(got) != 1 || got[0] != "rb" {
		t.Fatalf("aliases = %v, want [rb]", got)
	}
}
