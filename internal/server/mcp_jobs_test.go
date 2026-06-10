package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcp_jobs_test.go covers the async-first MCP lifecycle flow (issue #134):
// fleet_up & co return a {job_id, done:false} handle immediately, completion/
// failure/warnings are polled via fleet_job_status, and wait:true restores the
// blocking behavior.

// startHub runs svc's hub loop for the test (jobs post state snapshots to it).
func startHub(t *testing.T, svc *service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go svc.hub.run(ctx)
	t.Cleanup(cancel)
}

// pollJobStatus polls fleet_job_status until the job leaves "running" (or the
// deadline passes), returning the terminal status.
func pollJobStatus(t *testing.T, cs *mcp.ClientSession, jobID string) FleetJobStatusOutput {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var js FleetJobStatusOutput
	for {
		callJSON(t, cs, "fleet_job_status", map[string]any{"job_id": jobID}, &js)
		if js.State != "running" {
			return js
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never left running", jobID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMCPUpAsyncJobLifecycle drives the full async flow: fleet_up returns a job
// handle while provisioning is still in flight, fleet_job_status reports it
// running, and after the job completes it reports succeeded with the final
// instance record.
func TestMCPUpAsyncJobLifecycle(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	release := make(chan struct{})
	orig := jobRunCreate
	jobRunCreate = func(fleetName, instanceName, remote, branch string, verbose bool, b fleet.BackendType) error {
		<-release // hold provisioning in flight until the test releases it
		return state.Update(func(st *state.State) error {
			inst, _ := st.Fleets[fleetName].GetInstance(instanceName)
			inst.Status = fleet.StatusRunning
			inst.ContainerID = "fake-cid"
			return nil
		})
	}
	defer func() { jobRunCreate = orig }()

	svc := newService()
	startHub(t, svc)
	cs := mcpConnect(t, svc)

	// The kickoff returns immediately (the job is parked on `release`).
	var out FleetJobOutput
	callJSON(t, cs, "fleet_up", map[string]any{"fleet": "alpha", "instance": "i1"}, &out)
	if out.Done || out.Success || out.JobID == "" {
		t.Fatalf("async fleet_up should return done=false with a job_id: %+v", out)
	}
	if out.Instance == nil || out.Instance.Status != "creating" {
		t.Fatalf("async fleet_up should return the transitional creating record: %+v", out.Instance)
	}

	// In flight: the job polls as running, and fleet_list shows creating.
	var js FleetJobStatusOutput
	callJSON(t, cs, "fleet_job_status", map[string]any{"job_id": out.JobID}, &js)
	if js.State != "running" || js.Kind != "create_instance" || js.Fleet != "alpha" || js.Instance != "i1" {
		t.Fatalf("in-flight job status mismatch: %+v", js)
	}
	var list FleetListOutput
	callJSON(t, cs, "fleet_list", map[string]any{"fleet": "alpha"}, &list)
	if len(list.Instances) != 1 || list.Instances[0].Status != "creating" {
		t.Fatalf("fleet_list should show the creating instance: %+v", list.Instances)
	}

	close(release)
	js = pollJobStatus(t, cs, out.JobID)
	if js.State != "succeeded" || js.Error != "" {
		t.Fatalf("finished job should be succeeded: %+v", js)
	}
	if js.Result == nil || js.Result.Status != "running" || js.Result.ContainerID != "fake-cid" {
		t.Fatalf("finished job should carry the final instance record: %+v", js.Result)
	}
}

// TestMCPUpWaitBlocksUntilDone asserts wait:true retains the old block-until-
// done semantics: one call, final record in hand.
func TestMCPUpWaitBlocksUntilDone(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	orig := jobRunCreate
	jobRunCreate = func(fleetName, instanceName, remote, branch string, verbose bool, b fleet.BackendType) error {
		return state.Update(func(st *state.State) error {
			inst, _ := st.Fleets[fleetName].GetInstance(instanceName)
			inst.Status = fleet.StatusRunning
			return nil
		})
	}
	defer func() { jobRunCreate = orig }()

	svc := newService()
	startHub(t, svc)
	cs := mcpConnect(t, svc)

	var out FleetJobOutput
	callJSON(t, cs, "fleet_up", map[string]any{"fleet": "alpha", "instance": "i1", "wait": true}, &out)
	if !out.Done || !out.Success || out.JobID == "" {
		t.Fatalf("blocking fleet_up should return done=true success=true: %+v", out)
	}
	if out.Instance == nil || out.Instance.Status != "running" {
		t.Fatalf("blocking fleet_up should return the final record: %+v", out.Instance)
	}
}

// TestMCPUpAsyncFailureSurfacesViaJobStatus asserts a failed async job is
// readable over MCP: state=failed with the job's error message (as data, not a
// tool error — pollers must be able to read it).
func TestMCPUpAsyncFailureSurfacesViaJobStatus(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	orig := jobRunCreate
	jobRunCreate = func(fleetName, instanceName, remote, branch string, verbose bool, b fleet.BackendType) error {
		return context.DeadlineExceeded // any error; the message must surface
	}
	defer func() { jobRunCreate = orig }()

	svc := newService()
	startHub(t, svc)
	cs := mcpConnect(t, svc)

	var out FleetJobOutput
	callJSON(t, cs, "fleet_up", map[string]any{"fleet": "alpha", "instance": "i1"}, &out)
	js := pollJobStatus(t, cs, out.JobID)
	if js.State != "failed" || !strings.Contains(js.Error, "deadline exceeded") {
		t.Fatalf("failed job should report state=failed with the error: %+v", js)
	}
}

// TestMCPJobStatusUnknownJob asserts an unknown job id is a tool error that
// points the caller at fleet_list as the fallback.
func TestMCPJobStatusUnknownJob(t *testing.T) {
	isolateFleetDir(t)
	cs := mcpConnect(t, newService())

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "fleet_job_status", Arguments: map[string]any{"job_id": "job-0-999"},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError || !strings.Contains(toolText(res), "not found") || !strings.Contains(toolText(res), "fleet_list") {
		t.Fatalf("want 'not found' tool error pointing at fleet_list, got IsError=%v text=%q", res.IsError, toolText(res))
	}
}

// TestMCPDownAsyncMarksDeleting drives an async fleet_down: the kickoff returns
// the record marked deleting (pollable via fleet_list), and after the job
// finishes the record is gone and the job reports succeeded.
func TestMCPDownAsyncMarksDeleting(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "i1", Status: fleet.StatusRunning, ContainerID: "c1"},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	release := make(chan struct{})
	orig := jobDownInstance
	jobDownInstance = func(inst *fleet.Instance) error {
		<-release // hold the teardown in flight
		return nil
	}
	defer func() { jobDownInstance = orig }()

	svc := newService()
	startHub(t, svc)
	cs := mcpConnect(t, svc)

	var out FleetJobOutput
	callJSON(t, cs, "fleet_down", map[string]any{"fleet": "alpha", "instance": "i1"}, &out)
	if out.Done || out.JobID == "" {
		t.Fatalf("async fleet_down should return done=false with a job_id: %+v", out)
	}
	if out.Instance == nil || out.Instance.Status != "deleting" {
		t.Fatalf("async fleet_down should return the record marked deleting: %+v", out.Instance)
	}

	close(release)
	js := pollJobStatus(t, cs, out.JobID)
	if js.State != "succeeded" {
		t.Fatalf("teardown job should succeed: %+v", js)
	}
	var list FleetListOutput
	callJSON(t, cs, "fleet_list", map[string]any{"fleet": "alpha"}, &list)
	if len(list.Instances) != 0 {
		t.Fatalf("instance record should be removed after fleet_down: %+v", list.Instances)
	}
}
