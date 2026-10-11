//go:build !nok8s

package server

import (
	"context"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A create on a Kubernetes connection the daemon does not have is refused up
// front (no FAILED record left behind, nothing remembered), from the local
// socket as from any client.
func TestCreateInstanceRefusesAnUnknownClusterBeforeAnyRecord(t *testing.T) {
	isolateFleetDir(t)
	ran := false
	orig := jobRunCreate
	jobRunCreate = func(context.Context, string, string, string, string, bool, fleet.BackendType) error {
		ran = true
		return nil
	}
	t.Cleanup(func() { jobRunCreate = orig })
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git"},
	}}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	err := createErr(t, client, &fleetgrpc.CreateInstanceRequest{
		Fleet: "alpha", Instance: "k1", Backend: fleetgrpc.BackendType_BACKEND_TYPE_KUBERNETES,
		KubeCluster: "minikube", RememberDefault: true,
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("create on an unknown connection: error = %v, want NotFound", err)
	}
	if ran {
		t.Fatal("a refused create still ran its provisioning job")
	}
	st, err := state.Load()
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	if _, err := st.Fleets["alpha"].GetInstance("k1"); err == nil {
		t.Fatal("a refused Kubernetes create left a record behind")
	}
	if got := storedDefaultBackend(t); got != "" {
		t.Fatalf("a refused create remembered default %q", got)
	}
}
