package server

import (
	"context"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// runLiveStatusPass seeds state, stubs the backend probe, and runs one pass
// against a live hub, returning the post-pass persisted status of alpha/i1.
func runLiveStatusPass(t *testing.T, persisted fleet.InstanceStatus, probe backend.LiveStatus) fleet.InstanceStatus {
	t.Helper()
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1", Status: persisted, ContainerID: "c1"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	orig := probeLiveStatus
	probeLiveStatus = func(inst *fleet.Instance) backend.LiveStatus { return probe }
	defer func() { probeLiveStatus = orig }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub()
	go h.run(ctx)

	liveStatusPass(h) // the persisted reconciliation (state.Update) is synchronous

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return st.Fleets["alpha"].Instances[0].Status
}

func TestLiveStatusPassReconcilesPersistedStatus(t *testing.T) {
	if got := runLiveStatusPass(t, fleet.StatusRunning, backend.LiveStatusStopped); got != fleet.StatusStopped {
		t.Fatalf("running + probe stopped: got %q, want stopped", got)
	}
	if got := runLiveStatusPass(t, fleet.StatusStopped, backend.LiveStatusRunning); got != fleet.StatusRunning {
		t.Fatalf("stopped + probe running: got %q, want running", got)
	}
}

func TestLiveStatusPassLeavesStatusOnInconclusiveProbe(t *testing.T) {
	// unknown/missing probes must not flip the persisted status.
	for _, ls := range []backend.LiveStatus{backend.LiveStatusUnknown, backend.LiveStatusMissing} {
		if got := runLiveStatusPass(t, fleet.StatusRunning, ls); got != fleet.StatusRunning {
			t.Fatalf("running + probe %v: got %q, want running (unchanged)", ls, got)
		}
	}
}
