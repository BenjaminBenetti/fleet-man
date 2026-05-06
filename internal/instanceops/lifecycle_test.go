package instanceops

import (
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

type fakeClient struct {
	startCalls []string
	stopCalls  []string
	startErr   error
	stopErr    error
}

func (f *fakeClient) Start(containerID string) error {
	f.startCalls = append(f.startCalls, containerID)
	return f.startErr
}

func (f *fakeClient) Stop(containerID string) error {
	f.stopCalls = append(f.stopCalls, containerID)
	return f.stopErr
}

func (f *fakeClient) Stateful() bool { return true }

func TestStopInstanceTransitionsRunningToStopped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	client := &fakeClient{}
	restore := stubLifecycleClient(client)
	defer restore()

	writeLifecycleState(t, fleet.StatusRunning)

	result, err := StopInstance("alpha", "agent-1")
	if err != nil {
		t.Fatalf("StopInstance() error = %v", err)
	}

	if !result.Changed {
		t.Fatal("StopInstance() Changed = false, want true")
	}
	if result.Status != fleet.StatusStopped {
		t.Fatalf("StopInstance() Status = %q, want %q", result.Status, fleet.StatusStopped)
	}
	if len(client.stopCalls) != 1 || client.stopCalls[0] != "abc123" {
		t.Fatalf("StopInstance() stop calls = %v, want [abc123]", client.stopCalls)
	}

	inst := loadLifecycleInstance(t)
	if inst.Status != fleet.StatusStopped {
		t.Fatalf("saved status = %q, want %q", inst.Status, fleet.StatusStopped)
	}
	if inst.ContainerID != "abc123" {
		t.Fatalf("saved container ID = %q, want abc123", inst.ContainerID)
	}
}

func TestStartInstanceTransitionsStoppedToRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	client := &fakeClient{}
	restore := stubLifecycleClient(client)
	defer restore()

	writeLifecycleState(t, fleet.StatusStopped)

	result, err := StartInstance("alpha", "agent-1")
	if err != nil {
		t.Fatalf("StartInstance() error = %v", err)
	}

	if !result.Changed {
		t.Fatal("StartInstance() Changed = false, want true")
	}
	if result.Status != fleet.StatusRunning {
		t.Fatalf("StartInstance() Status = %q, want %q", result.Status, fleet.StatusRunning)
	}
	if len(client.startCalls) != 1 || client.startCalls[0] != "abc123" {
		t.Fatalf("StartInstance() start calls = %v, want [abc123]", client.startCalls)
	}

	inst := loadLifecycleInstance(t)
	if inst.Status != fleet.StatusRunning {
		t.Fatalf("saved status = %q, want %q", inst.Status, fleet.StatusRunning)
	}
}

func TestStopInstanceNoOpsWhenAlreadyStopped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	client := &fakeClient{}
	restore := stubLifecycleClient(client)
	defer restore()

	writeLifecycleState(t, fleet.StatusStopped)

	result, err := StopInstance("alpha", "agent-1")
	if err != nil {
		t.Fatalf("StopInstance() error = %v", err)
	}

	if result.Changed {
		t.Fatal("StopInstance() Changed = true, want false")
	}
	if len(client.stopCalls) != 0 {
		t.Fatalf("StopInstance() stop calls = %v, want none", client.stopCalls)
	}
}

func TestStartInstanceNoOpsWhenAlreadyRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	client := &fakeClient{}
	restore := stubLifecycleClient(client)
	defer restore()

	writeLifecycleState(t, fleet.StatusRunning)

	result, err := StartInstance("alpha", "agent-1")
	if err != nil {
		t.Fatalf("StartInstance() error = %v", err)
	}

	if result.Changed {
		t.Fatal("StartInstance() Changed = true, want false")
	}
	if len(client.startCalls) != 0 {
		t.Fatalf("StartInstance() start calls = %v, want none", client.startCalls)
	}
}

func stubLifecycleClient(client containerController) func() {
	prev := newClient
	newClient = func(bt fleet.BackendType) containerController { return client }
	return func() {
		newClient = prev
	}
}

func writeLifecycleState(t *testing.T, status fleet.InstanceStatus) {
	t.Helper()

	st := &state.State{
		Fleets: map[string]*fleet.Fleet{
			"alpha": {
				Name:   "alpha",
				Remote: "git@github.com:org/alpha.git",
				Instances: []*fleet.Instance{
					{
						Name:         "agent-1",
						ContainerID:  "abc123",
						WorkspaceDir: "/tmp/alpha/agent-1/alpha",
						CreatedAt:    time.Unix(0, 0),
						Status:       status,
					},
				},
			},
		},
	}

	if err := state.Save(st); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}
}

func loadLifecycleInstance(t *testing.T) *fleet.Instance {
	t.Helper()

	st, err := state.Load()
	if err != nil {
		t.Fatalf("state.Load() error = %v", err)
	}

	f := st.Fleets["alpha"]
	if f == nil {
		t.Fatal("fleet alpha not found after load")
	}

	inst, err := f.GetInstance("agent-1")
	if err != nil {
		t.Fatalf("GetInstance() error = %v", err)
	}
	return inst
}
