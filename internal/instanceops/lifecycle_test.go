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

	instance := loadLifecycleInstance(t)
	if instance.Status != fleet.StatusStopped {
		t.Fatalf("saved status = %q, want %q", instance.Status, fleet.StatusStopped)
	}
	if instance.ContainerID != "abc123" {
		t.Fatalf("saved container ID = %q, want abc123", instance.ContainerID)
	}
}

// TestStopInstanceRecordsLastBranch locks the read-before-stop: the branch the
// workspace is on is saved as LastBranch with the stopped status, since a
// workspace that lives only in the running instance can't be read afterwards.
// A read that fails (or finds a detached HEAD) keeps the previous value.
func TestStopInstanceRecordsLastBranch(t *testing.T) {
	tests := []struct {
		name   string
		branch string
		ok     bool
		want   string
	}{
		{"read before stop", "feature/x", true, "feature/x"},
		{"detached head keeps the last named branch", "", true, "main"},
		{"failed read keeps the last named branch", "", false, "main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			client := &fakeClient{}
			restore := stubLifecycleClient(client)
			defer restore()
			writeLifecycleState(t, fleet.StatusRunning)
			if err := state.Update(func(st *state.State) error {
				st.Fleets["alpha"].Instances[0].LastBranch = "main"
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			var readBeforeStop bool
			readWorkspaceBranch = func(inst *fleet.Instance) (string, bool) {
				readBeforeStop = len(client.stopCalls) == 0
				return tt.branch, tt.ok
			}

			if _, err := StopInstance("alpha", "agent-1"); err != nil {
				t.Fatalf("StopInstance() error = %v", err)
			}
			if !readBeforeStop {
				t.Fatal("the branch must be read before the backend stops the instance")
			}
			if got := loadLifecycleInstance(t).LastBranch; got != tt.want {
				t.Fatalf("saved LastBranch = %q, want %q", got, tt.want)
			}
		})
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

	instance := loadLifecycleInstance(t)
	if instance.Status != fleet.StatusRunning {
		t.Fatalf("saved status = %q, want %q", instance.Status, fleet.StatusRunning)
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

// slowClient runs during as its Start or Stop works.
type slowClient struct{ during func() }

func (c slowClient) Start(string) error { c.during(); return nil }
func (c slowClient) Stop(string) error  { c.during(); return nil }

// TestTransitionKeepsWritesMadeMeanwhile: a backend's start or stop can take
// minutes (a Kubernetes pod waits for its node and image), and what the rest
// of the daemon wrote to state.json meanwhile (another instance's create, a
// change to this record's neighbours) survives the transition's own write.
// A record a destroy took over meanwhile is left to the destroy.
func TestTransitionKeepsWritesMadeMeanwhile(t *testing.T) {
	for _, tc := range []struct {
		name string
		from fleet.InstanceStatus
		run  func(fleetName, instanceName string) (*Result, error)
		want fleet.InstanceStatus
	}{
		{"start", fleet.StatusStopped, StartInstance, fleet.StatusRunning},
		{"stop", fleet.StatusRunning, StopInstance, fleet.StatusStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			restore := stubLifecycleClient(slowClient{during: func() {
				if err := state.Update(func(st *state.State) error {
					f := st.Fleets["alpha"]
					f.Instances = append(f.Instances, &fleet.Instance{Name: "agent-2", Status: fleet.StatusCreating})
					f.Instances[0].Tag = "set meanwhile"
					return nil
				}); err != nil {
					t.Error(err)
				}
			}})
			defer restore()
			writeLifecycleState(t, tc.from)

			if _, err := tc.run("alpha", "agent-1"); err != nil {
				t.Fatal(err)
			}
			st, err := state.Load()
			if err != nil {
				t.Fatal(err)
			}
			f := st.Fleets["alpha"]
			if other, err := f.GetInstance("agent-2"); err != nil || other.Status != fleet.StatusCreating {
				t.Fatalf("the record created meanwhile: %+v, %v", other, err)
			}
			if inst := loadLifecycleInstance(t); inst.Status != tc.want || inst.Tag != "set meanwhile" {
				t.Fatalf("record %+v, want status %s with the tag set meanwhile", inst, tc.want)
			}
		})
	}

	t.Run("destroyed meanwhile", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		restore := stubLifecycleClient(slowClient{during: func() {
			_ = state.Update(func(st *state.State) error {
				st.Fleets["alpha"].Instances[0].Status = fleet.StatusDeleting
				return nil
			})
		}})
		defer restore()
		writeLifecycleState(t, fleet.StatusStopped)
		if _, err := StartInstance("alpha", "agent-1"); err == nil {
			t.Fatal("a start whose record a destroy took reported success")
		}
		if inst := loadLifecycleInstance(t); inst.Status != fleet.StatusDeleting {
			t.Fatalf("status %s, want the destroy's", inst.Status)
		}
	})
}

// TestTransitionsBindControllerToInstanceRecord pins the stop/start
// construction site: the controller is built for the persisted instance record
// (backendutil.NewForInstance), not for a bare backend type, so a backend whose
// handle needs more of the instance's identity than its type gets it.
func TestTransitionsBindControllerToInstanceRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	client := &fakeClient{}
	restore := stubLifecycleClient(client)
	defer restore()
	var bound []*fleet.Instance
	newClient = func(instance *fleet.Instance) containerController {
		bound = append(bound, instance)
		return client
	}

	writeLifecycleState(t, fleet.StatusRunning)
	if _, err := StopInstance("alpha", "agent-1"); err != nil {
		t.Fatalf("StopInstance() error = %v", err)
	}
	if _, err := StartInstance("alpha", "agent-1"); err != nil {
		t.Fatalf("StartInstance() error = %v", err)
	}

	if len(bound) != 2 {
		t.Fatalf("controller built %d times, want 2 (stop + start)", len(bound))
	}
	for _, instance := range bound {
		if instance.Name != "agent-1" || instance.ContainerID != "abc123" || instance.WorkspaceDir != "/tmp/alpha/agent-1/alpha" {
			t.Fatalf("controller bound to %+v, want the persisted agent-1 record", instance)
		}
	}
}

// stubLifecycleClient swaps the package-level client + post-start hook
// for test doubles. Both are reset together because every transition
// test goes through the same path: build a client, call Start/Stop, run
// the post-start hook. Letting the real hook run would have it reach
// for a host backend and a real container, neither of which exists in
// these tests.
func stubLifecycleClient(client containerController) func() {
	prevClient := newClient
	prevHook := postStartHook
	prevBranch := readWorkspaceBranch
	newClient = func(*fleet.Instance) containerController { return client }
	postStartHook = func(string, *fleet.Instance, string) {}
	readWorkspaceBranch = func(*fleet.Instance) (string, bool) { return "", false }
	return func() {
		newClient = prevClient
		postStartHook = prevHook
		readWorkspaceBranch = prevBranch
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

	instance, err := f.GetInstance("agent-1")
	if err != nil {
		t.Fatalf("GetInstance() error = %v", err)
	}
	return instance
}
