package tui

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// liveStatusFixture builds a state with a single instance in the
// requested status. HOME is repointed via t.TempDir() so the global
// state.Save mutex doesn't trip on a real ~/.fleet directory.
func liveStatusFixture(t *testing.T, status fleet.InstanceStatus) *state.State {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return &state.State{
		Fleets: map[string]*fleet.Fleet{
			"alpha": {
				Name:   "alpha",
				Remote: "git@github.com:org/alpha.git",
				Instances: []*fleet.Instance{
					{
						Name:        "agent-1",
						ContainerID: "abc123",
						Status:      status,
						Backend:     fleet.BackendDevcontainer,
					},
				},
			},
		},
	}
}

func TestApplyLiveStatusesFlipsRunningToStopped(t *testing.T) {
	st := liveStatusFixture(t, fleet.StatusRunning)

	changed := applyLiveStatuses(st, map[string]backend.LiveStatus{
		"alpha/agent-1": backend.LiveStatusStopped,
	})

	if !changed {
		t.Fatal("applyLiveStatuses() = false, want true")
	}
	if got := st.Fleets["alpha"].Instances[0].Status; got != fleet.StatusStopped {
		t.Fatalf("status = %q, want %q", got, fleet.StatusStopped)
	}
}

func TestApplyLiveStatusesFlipsStoppedToRunningAndClearsError(t *testing.T) {
	st := liveStatusFixture(t, fleet.StatusStopped)
	st.Fleets["alpha"].Instances[0].Error = "stale"

	changed := applyLiveStatuses(st, map[string]backend.LiveStatus{
		"alpha/agent-1": backend.LiveStatusRunning,
	})

	if !changed {
		t.Fatal("applyLiveStatuses() = false, want true")
	}
	inst := st.Fleets["alpha"].Instances[0]
	if inst.Status != fleet.StatusRunning {
		t.Fatalf("status = %q, want %q", inst.Status, fleet.StatusRunning)
	}
	if inst.Error != "" {
		t.Fatalf("error = %q, want cleared", inst.Error)
	}
}

func TestApplyLiveStatusesPreservesTransientStates(t *testing.T) {
	transient := []fleet.InstanceStatus{
		fleet.StatusCreating,
		fleet.StatusStarting,
		fleet.StatusStopping,
		fleet.StatusDeleting,
		fleet.StatusFailed,
	}
	for _, status := range transient {
		t.Run(string(status), func(t *testing.T) {
			st := liveStatusFixture(t, status)

			changed := applyLiveStatuses(st, map[string]backend.LiveStatus{
				"alpha/agent-1": backend.LiveStatusStopped,
			})

			if changed {
				t.Fatal("applyLiveStatuses() = true, want false (transient must not be overridden)")
			}
			if got := st.Fleets["alpha"].Instances[0].Status; got != status {
				t.Fatalf("status = %q, want preserved %q", got, status)
			}
		})
	}
}

func TestApplyLiveStatusesIgnoresUnknownAndMissing(t *testing.T) {
	for _, liveStatus := range []backend.LiveStatus{backend.LiveStatusUnknown, backend.LiveStatusMissing} {
		t.Run(string(liveStatus), func(t *testing.T) {
			st := liveStatusFixture(t, fleet.StatusRunning)

			changed := applyLiveStatuses(st, map[string]backend.LiveStatus{
				"alpha/agent-1": liveStatus,
			})

			if changed {
				t.Fatalf("applyLiveStatuses() = true, want false for %q", liveStatus)
			}
			if got := st.Fleets["alpha"].Instances[0].Status; got != fleet.StatusRunning {
				t.Fatalf("status = %q, want preserved running", got)
			}
		})
	}
}

func TestCollectLiveStatusProbesSkipsTransientAndEmptyContainerID(t *testing.T) {
	st := &state.State{
		Fleets: map[string]*fleet.Fleet{
			"alpha": {
				Name: "alpha",
				Instances: []*fleet.Instance{
					{Name: "running-keep", ContainerID: "c1", Status: fleet.StatusRunning, Backend: fleet.BackendDevcontainer},
					{Name: "stopped-keep", ContainerID: "c2", Status: fleet.StatusStopped, Backend: fleet.BackendDevcontainer},
					{Name: "creating-skip", ContainerID: "c3", Status: fleet.StatusCreating, Backend: fleet.BackendDevcontainer},
					{Name: "starting-skip", ContainerID: "c4", Status: fleet.StatusStarting, Backend: fleet.BackendDevcontainer},
					{Name: "stopping-skip", ContainerID: "c5", Status: fleet.StatusStopping, Backend: fleet.BackendDevcontainer},
					{Name: "deleting-skip", ContainerID: "c6", Status: fleet.StatusDeleting, Backend: fleet.BackendDevcontainer},
					{Name: "failed-skip", ContainerID: "c7", Status: fleet.StatusFailed, Backend: fleet.BackendDevcontainer},
					{Name: "no-id-skip", ContainerID: "", Status: fleet.StatusRunning, Backend: fleet.BackendDevcontainer},
				},
			},
		},
	}

	probes := collectLiveStatusProbes(st)
	if len(probes) != 2 {
		t.Fatalf("len(probes) = %d, want 2; got %+v", len(probes), probes)
	}
	got := map[string]bool{}
	for _, p := range probes {
		got[p.instanceName] = true
	}
	if !got["running-keep"] || !got["stopped-keep"] {
		t.Fatalf("probes missing expected instances; got %+v", got)
	}
}
