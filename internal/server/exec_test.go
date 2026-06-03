package server

import (
	"context"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestResolveExecCommandReturnsBackendArgv(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "i1", Backend: fleet.BackendDevcontainer, WorkspaceDir: "/ws/alpha/i1", ContainerID: "c1"},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := newService()

	reply, err := svc.ResolveExecCommand(context.Background(), &fleetgrpc.ResolveExecCommandRequest{
		Fleet: "alpha", Instance: "i1", Argv: []string{"echo", "hi"},
	})
	if err != nil {
		t.Fatalf("ResolveExecCommand: %v", err)
	}
	argv := reply.GetArgv()
	if len(argv) == 0 || argv[0] != "devcontainer" {
		t.Fatalf("want devcontainer argv, got %v", argv)
	}
	if joined := strings.Join(argv, " "); !strings.Contains(joined, "echo") || !strings.Contains(joined, "hi") {
		t.Fatalf("argv missing requested command: %v", argv)
	}

	// Unknown fleet/instance -> NotFound (so a typo fails fast for the client).
	if _, err := svc.ResolveExecCommand(context.Background(), &fleetgrpc.ResolveExecCommandRequest{Fleet: "ghost", Instance: "x"}); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
	if _, err := svc.ResolveExecCommand(context.Background(), &fleetgrpc.ResolveExecCommandRequest{Fleet: "alpha", Instance: "ghost"}); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for missing instance, got %v", err)
	}
}
