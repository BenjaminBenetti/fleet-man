package server

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// newTestServer stands up the service on an in-process bufconn with the hub
// running, returning a connected client and a cleanup func.
func newTestServer(t *testing.T) (*service, fleetgrpc.FleetServiceClient, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	svc := newService()
	go svc.hub.run(ctx)

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gs, svc)
	go func() { _ = gs.Serve(lis) }()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cleanup := func() {
		_ = conn.Close()
		gs.Stop()
		cancel()
	}
	return svc, fleetgrpc.NewFleetServiceClient(conn), cleanup
}

// drainJob reads a job stream to its JobDone, returning the ordered events.
func drainJob(t *testing.T, stream grpc.ServerStreamingClient[fleetgrpc.JobEvent]) []*fleetgrpc.JobEvent {
	t.Helper()
	var evs []*fleetgrpc.JobEvent
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return evs
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		evs = append(evs, ev)
		if ev.GetDone() != nil {
			return evs
		}
	}
}

func TestCreateInstanceJobStartedThenDone(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	orig := jobRunCreate
	jobRunCreate = func(fleetName, instanceName, remote, branch string, verbose bool, b fleet.BackendType) error {
		if remote != "git@x:a.git" {
			t.Errorf("remote not resolved from fleet record: %q", remote)
		}
		return state.Update(func(st *state.State) error {
			if f, ok := st.Fleets[fleetName]; ok {
				if inst, err := f.GetInstance(instanceName); err == nil {
					inst.Status = fleet.StatusRunning
					inst.ContainerID = "fake-cid"
				}
			}
			return nil
		})
	}
	defer func() { jobRunCreate = orig }()

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.CreateInstance(context.Background(), &fleetgrpc.CreateInstanceRequest{
		Fleet: "alpha", Instance: "i1", Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	evs := drainJob(t, stream)

	if len(evs) < 2 {
		t.Fatalf("want >=2 events, got %d", len(evs))
	}
	if evs[0].GetStarted() == nil {
		t.Fatalf("first event must be JobStarted, got %T", evs[0].GetEvent())
	}
	last := evs[len(evs)-1].GetDone()
	if last == nil || !last.GetSuccess() {
		t.Fatalf("last event must be successful JobDone: %v", evs[len(evs)-1])
	}
	if last.GetInstance().GetStatus() != fleetgrpc.InstanceStatus_INSTANCE_STATUS_RUNNING {
		t.Fatalf("JobDone instance not RUNNING: %v", last.GetInstance())
	}

	st, _ := state.Load()
	inst, err := st.Fleets["alpha"].GetInstance("i1")
	if err != nil || inst.Status != fleet.StatusRunning || inst.ContainerID != "fake-cid" {
		t.Fatalf("persisted record wrong: %+v err=%v", inst, err)
	}
}

func TestCreateInstanceRejectsDuplicate(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git", Instances: []*fleet.Instance{{Name: "i1"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.CreateInstance(context.Background(), &fleetgrpc.CreateInstanceRequest{Fleet: "alpha", Instance: "i1"})
	if err != nil {
		t.Fatalf("CreateInstance call: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("want AlreadyExists, got %v", err)
	}
}

func TestStartStopJobs(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1", Status: fleet.StatusStopped, ContainerID: "c1"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	origStart, origStop := jobRunStart, jobRunStop
	jobRunStart = func(f, i string) error {
		return state.Update(func(st *state.State) error {
			inst, _ := st.Fleets[f].GetInstance(i)
			inst.Status = fleet.StatusRunning
			return nil
		})
	}
	jobRunStop = func(f, i string) error {
		return state.Update(func(st *state.State) error {
			inst, _ := st.Fleets[f].GetInstance(i)
			inst.Status = fleet.StatusStopped
			return nil
		})
	}
	defer func() { jobRunStart, jobRunStop = origStart, origStop }()

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	startStream, err := client.StartInstance(context.Background(), &fleetgrpc.StartInstanceRequest{Fleet: "alpha", Instance: "i1"})
	if err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	evs := drainJob(t, startStream)
	if d := evs[len(evs)-1].GetDone(); d == nil || !d.GetSuccess() {
		t.Fatalf("start job not done-success: %v", evs)
	}
	if st, _ := state.Load(); st.Fleets["alpha"].Instances[0].Status != fleet.StatusRunning {
		t.Fatalf("instance not running after start")
	}

	stopStream, err := client.StopInstance(context.Background(), &fleetgrpc.StopInstanceRequest{Fleet: "alpha", Instance: "i1"})
	if err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	drainJob(t, stopStream)
	if st, _ := state.Load(); st.Fleets["alpha"].Instances[0].Status != fleet.StatusStopped {
		t.Fatalf("instance not stopped after stop")
	}
}

func TestDestroyInstanceJobRemovesRecord(t *testing.T) {
	isolateFleetDir(t)
	wsDir := t.TempDir()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1", WorkspaceDir: wsDir}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.DestroyInstance(context.Background(), &fleetgrpc.DestroyInstanceRequest{Fleet: "alpha", Instance: ptr("i1")})
	if err != nil {
		t.Fatalf("DestroyInstance: %v", err)
	}
	evs := drainJob(t, stream)
	if d := evs[len(evs)-1].GetDone(); d == nil || !d.GetSuccess() {
		t.Fatalf("destroy not done-success: %v", evs)
	}
	st, _ := state.Load()
	if _, err := st.Fleets["alpha"].GetInstance("i1"); err == nil {
		t.Fatalf("instance record not removed")
	}
}

// TestJobManagerFinishedRetention asserts a finished job stays resolvable by id
// (for async pollers) until FIFO eviction pushes it out.
func TestJobManagerFinishedRetention(t *testing.T) {
	m := newJobManager()

	first := m.start(fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "f", "i0", time.Now())
	firstID := first.summary.GetJobId()
	if m.get(firstID) != first {
		t.Fatalf("active job not resolvable by id")
	}
	m.finish(firstID)
	if m.get(firstID) != first {
		t.Fatalf("finished job should stay resolvable by id")
	}
	if len(m.summaries()) != 0 {
		t.Fatalf("finished job must not be advertised as active")
	}

	// Filling the retention window evicts the oldest finished job.
	for range finishedJobRetention {
		j := m.start(fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "f", "i", time.Now())
		m.finish(j.summary.GetJobId())
	}
	if m.get(firstID) != nil {
		t.Fatalf("oldest finished job should be evicted after %d newer ones", finishedJobRetention)
	}
}

func TestActiveJobsSurfacedInGetState(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1", Status: fleet.StatusStopped, ContainerID: "c1"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	release := make(chan struct{})
	orig := jobRunStart
	jobRunStart = func(f, i string) error {
		<-release // hold the job in-flight until the test releases it
		return nil
	}
	defer func() { jobRunStart = orig }()

	_, client, cleanup := newTestServer(t)
	defer cleanup()

	stream, err := client.StartInstance(context.Background(), &fleetgrpc.StartInstanceRequest{Fleet: "alpha", Instance: "i1"})
	if err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	// First event (JobStarted) confirms the job is registered + in-flight.
	if ev, err := stream.Recv(); err != nil || ev.GetStarted() == nil {
		t.Fatalf("want JobStarted, got ev=%v err=%v", ev, err)
	}

	// GetState must advertise the in-flight job so a non-launching watcher can
	// learn its identity.
	var found bool
	for i := 0; i < 50 && !found; i++ {
		reply, err := client.GetState(context.Background(), &fleetgrpc.GetStateRequest{})
		if err != nil {
			t.Fatalf("GetState: %v", err)
		}
		for _, js := range reply.GetActiveJobs() {
			if js.GetFleet() == "alpha" && js.GetInstance() == "i1" && js.GetKind() == fleetgrpc.JobKind_JOB_KIND_START_INSTANCE {
				found = true
			}
		}
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Fatalf("in-flight job not surfaced in GetState.active_jobs")
	}

	close(release)
	drainJob(t, stream)

	// After completion the job is no longer active.
	reply, _ := client.GetState(context.Background(), &fleetgrpc.GetStateRequest{})
	if len(reply.GetActiveJobs()) != 0 {
		t.Fatalf("completed job still active: %v", reply.GetActiveJobs())
	}
}
