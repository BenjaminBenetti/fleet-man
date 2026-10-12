package server

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// stubCreateWithSteps makes jobRunCreate report a step, write output with a
// secret -e value, then wait on release before marking the instance running.
func stubCreateWithSteps(t *testing.T, release <-chan struct{}) {
	t.Helper()
	orig := jobRunCreate
	jobRunCreate = func(ctx context.Context, fleetName, instanceName, _, _ string, _ bool, _ fleet.BackendType) error {
		backend.ReportStep(ctx, fleet.StepBackendUp, "devcontainer")
		_, _ = io.WriteString(backend.JobLogFrom(ctx), "Run: docker run -e API_KEY=sk-live-secret img\n")
		<-release
		return state.Update(func(st *state.State) error {
			inst, _ := st.Fleets[fleetName].GetInstance(instanceName)
			inst.Status = fleet.StatusRunning
			inst.ContainerID = "fake-cid"
			return nil
		})
	}
	t.Cleanup(func() { jobRunCreate = orig })
}

func seedAlpha(t *testing.T) {
	t.Helper()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// nextActiveJobs reads the Watch stream to its next ActiveJobs event.
func nextActiveJobs(t *testing.T, watch fleetgrpc.FleetService_WatchClient) *fleetgrpc.ActiveJobs {
	t.Helper()
	for {
		ev, err := watch.Recv()
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		if aj := ev.GetActiveJobs(); aj != nil {
			return aj
		}
	}
}

// TestCreateJobStreamsStepsAndLogsAndWatchShowsTheStep: a create's step and
// output reach its job stream and the instance's creation log, redacted, and
// every Watch subscriber sees the job's current step until it ends.
func TestCreateJobStreamsStepsAndLogsAndWatchShowsTheStep(t *testing.T) {
	isolateFleetDir(t)
	seedAlpha(t)
	release := make(chan struct{})
	stubCreateWithSteps(t, release)

	_, client, cleanup := newTestServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	watch, err := client.Watch(ctx, &fleetgrpc.WatchRequest{IncludeInitialState: true})
	if err != nil {
		t.Fatal(err)
	}
	if initial := nextActiveJobs(t, watch); len(initial.GetJobs()) != 0 {
		t.Fatalf("initial ActiveJobs = %v, want none", initial)
	}

	stream, err := client.CreateInstance(ctx, &fleetgrpc.CreateInstanceRequest{Fleet: "alpha", Instance: "i1", Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER})
	if err != nil {
		t.Fatal(err)
	}

	// Watch reports the job, then its step.
	var job *fleetgrpc.JobSummary
	for job == nil || job.GetCurrentStep() != fleetgrpc.JobStep_JOB_STEP_BACKEND_UP {
		aj := nextActiveJobs(t, watch)
		if len(aj.GetJobs()) != 1 {
			t.Fatalf("ActiveJobs = %v, want the one create", aj)
		}
		job = aj.GetJobs()[0]
	}
	if job.GetFleet() != "alpha" || job.GetInstance() != "i1" || job.GetKind() != fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE {
		t.Fatalf("active job = %v", job)
	}
	// GetState agrees.
	st, err := client.GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.GetActiveJobs()) != 1 || st.GetActiveJobs()[0].GetCurrentStep() != fleetgrpc.JobStep_JOB_STEP_BACKEND_UP {
		t.Fatalf("GetState active jobs = %v", st.GetActiveJobs())
	}
	// The creation log exists while the instance is still creating.
	if log := readCreationLog(t); !strings.Contains(log, "==> starting container: devcontainer") || !strings.Contains(log, "API_KEY=<redacted>") {
		t.Fatalf("creation log while creating:\n%s", log)
	}

	close(release)
	evs := drainJob(t, stream)
	if d := evs[len(evs)-1].GetDone(); d == nil || !d.GetSuccess() {
		t.Fatalf("create did not succeed: %v", evs[len(evs)-1])
	}
	var sawStep, sawLine bool
	for _, ev := range evs {
		if ev.GetProgress().GetStep() == fleetgrpc.JobStep_JOB_STEP_BACKEND_UP {
			sawStep = true
		}
		if line := ev.GetLog().GetLine(); strings.Contains(line, "docker run") {
			sawLine = true
			if strings.Contains(line, "sk-live-secret") {
				t.Fatalf("job stream leaks the -e value: %q", line)
			}
		}
	}
	if !sawStep || !sawLine {
		t.Fatalf("job stream missing the step (%v) or the output (%v)", sawStep, sawLine)
	}

	// Once the job ends Watch drops it.
	for aj := nextActiveJobs(t, watch); len(aj.GetJobs()) != 0; aj = nextActiveJobs(t, watch) {
	}

	log := readCreationLog(t)
	if strings.Contains(log, "sk-live-secret") || !strings.HasSuffix(log, "==> done\n") {
		t.Fatalf("final creation log:\n%s", log)
	}

	// Destroying the instance removes its creation log. Its fake container
	// is never torn down for real: that would call the host's docker, whose
	// load would then decide whether the test meets its deadline.
	origDown := jobDownInstance
	jobDownInstance = func(*fleet.Instance) error { return nil }
	t.Cleanup(func() { jobDownInstance = origDown })
	dstream, err := client.DestroyInstance(ctx, &fleetgrpc.DestroyInstanceRequest{Fleet: "alpha", Instance: ptr("i1")})
	if err != nil {
		t.Fatal(err)
	}
	drainJob(t, dstream)
	if _, err := os.Stat(state.InstanceCreationLogPath("alpha", "i1")); !os.IsNotExist(err) {
		t.Fatalf("creation log survived the destroy: %v", err)
	}
}

// TestMCPJobStatusReportsTheStep: fleet_job_status names the step a running
// provisioning job is on.
func TestMCPJobStatusReportsTheStep(t *testing.T) {
	isolateFleetDir(t)
	seedAlpha(t)
	release := make(chan struct{})
	stubCreateWithSteps(t, release)

	svc := newService()
	startHub(t, svc)
	cs := mcpConnect(t, svc)

	var out FleetJobOutput
	callJSON(t, cs, "fleet_up", map[string]any{"fleet": "alpha", "instance": "i1"}, &out)
	deadline := time.Now().Add(5 * time.Second)
	var js FleetJobStatusOutput
	for {
		callJSON(t, cs, "fleet_job_status", map[string]any{"job_id": out.JobID}, &js)
		if js.Step == "starting container" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fleet_job_status never reported the step: %+v", js)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	if final := pollJobStatus(t, cs, out.JobID); final.State != "succeeded" {
		t.Fatalf("final status = %+v, want succeeded", final)
	}
	var done FleetJobStatusOutput
	callJSON(t, cs, "fleet_job_status", map[string]any{"job_id": out.JobID}, &done)
	if done.Step != "" {
		t.Fatalf("a finished job still reports step %q", done.Step)
	}
}

// TestCloneAndRebuildJobsWriteTheCreationLog: the clone job logs to the new
// instance's creation log and the rebuild job to the rebuilt one's, each with
// the steps its run reported, and the run gets the log on its ctx.
func TestCloneAndRebuildJobsWriteTheCreationLog(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "src", Status: fleet.StatusRunning, ContainerID: "c1"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	markRunning := func(f, i, cid string) error {
		return state.Update(func(st *state.State) error {
			inst, _ := st.Fleets[f].GetInstance(i)
			inst.Status, inst.ContainerID = fleet.StatusRunning, cid
			return nil
		})
	}
	origClone, origRebuild := jobRunClone, jobRunRebuild
	jobRunClone = func(ctx context.Context, f, src, dest string, _ bool) error {
		backend.ReportStep(ctx, fleet.StepBackendClone, "from "+src)
		return markRunning(f, dest, "c2")
	}
	jobRunRebuild = func(ctx context.Context, f, i string) error {
		backend.ReportStep(ctx, fleet.StepBackendUp, "rebuild")
		return markRunning(f, i, "c3")
	}
	t.Cleanup(func() { jobRunClone, jobRunRebuild = origClone, origRebuild })

	_, client, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()

	cstream, err := client.CloneInstance(ctx, &fleetgrpc.CloneInstanceRequest{Fleet: "alpha", SourceInstance: "src", NewInstance: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	drainJob(t, cstream)
	data, err := os.ReadFile(state.InstanceCreationLogPath("alpha", "copy"))
	if err != nil {
		t.Fatalf("clone wrote no creation log: %v", err)
	}
	if log := string(data); !strings.Contains(log, "==> fleet clone of src: alpha/copy") || !strings.Contains(log, "==> cloning container: from src") {
		t.Fatalf("clone creation log:\n%s", log)
	}

	rstream, err := client.RebuildInstance(ctx, &fleetgrpc.RebuildInstanceRequest{Fleet: "alpha", Instance: "src"})
	if err != nil {
		t.Fatal(err)
	}
	drainJob(t, rstream)
	data, err = os.ReadFile(state.InstanceCreationLogPath("alpha", "src"))
	if err != nil {
		t.Fatalf("rebuild wrote no creation log: %v", err)
	}
	if log := string(data); !strings.Contains(log, "==> fleet rebuild: alpha/src") || !strings.Contains(log, "==> starting container: rebuild") || !strings.HasSuffix(log, "==> done\n") {
		t.Fatalf("rebuild creation log:\n%s", log)
	}
}
