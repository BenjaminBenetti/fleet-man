package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/svcruntime/svcruntimetest"
)

// provisioningStub replaces the create, clone and rebuild seams with runs that
// block until their job is cancelled (or released, when ignoreCancel is set,
// for a run wedged in a backend call that takes no context). Each run reports
// what it saw when it stopped on its channel.
type provisioningStub struct {
	ignoreCancel bool
	release      chan struct{}
	entered      chan string     // "fleet/instance" once a run is in flight
	stopped      chan stubResult // one per run, when it returns
}

type stubResult struct {
	key    string
	cause  error                // context.Cause of the run's context
	status fleet.InstanceStatus // the record's status when the run stopped ("" if gone)
}

func stubProvisioning(t *testing.T, ignoreCancel bool) *provisioningStub {
	t.Helper()
	p := &provisioningStub{
		ignoreCancel: ignoreCancel,
		release:      make(chan struct{}),
		entered:      make(chan string, 16),
		stopped:      make(chan stubResult, 16),
	}
	run := func(ctx context.Context, fleetName, instanceName string) error {
		key := fleetName + "/" + instanceName
		p.entered <- key
		if p.ignoreCancel {
			<-p.release
		} else {
			select {
			case <-ctx.Done():
			case <-p.release:
			}
		}
		res := stubResult{key: key, cause: context.Cause(ctx)}
		if st, err := state.Load(); err == nil {
			if inst := findInstance(st, fleetName, instanceName); inst != nil {
				res.status = inst.Status
			}
		}
		p.stopped <- res
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled: %w", context.Cause(ctx))
		}
		return nil
	}
	// The destroys under test must not reach docker.
	origDown := jobDownInstance
	jobDownInstance = func(*fleet.Instance) error { return nil }
	t.Cleanup(func() { jobDownInstance = origDown })
	origCreate, origClone, origRebuild := jobRunCreate, jobRunClone, jobRunRebuild
	jobRunCreate = func(ctx context.Context, fleetName, instanceName, _, _ string, _ bool, _ fleet.BackendType) error {
		return run(ctx, fleetName, instanceName)
	}
	jobRunClone = func(ctx context.Context, fleetName, _, destInstance string, _ bool) error {
		return run(ctx, fleetName, destInstance)
	}
	jobRunRebuild = func(ctx context.Context, fleetName, instanceName string) error {
		return run(ctx, fleetName, instanceName)
	}
	t.Cleanup(func() { jobRunCreate, jobRunClone, jobRunRebuild = origCreate, origClone, origRebuild })
	return p
}

func (p *provisioningStub) awaitEntered(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-p.entered:
		if got != want {
			t.Fatalf("run entered for %s, want %s", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("run for %s never started", want)
	}
}

func (p *provisioningStub) awaitStopped(t *testing.T) stubResult {
	t.Helper()
	select {
	case r := <-p.stopped:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("run never stopped")
		return stubResult{}
	}
}

// startProvisioning starts one create, clone or rebuild job for alpha/<inst>
// through the service, returning the job.
func startProvisioning(t *testing.T, s *service, kind fleetgrpc.JobKind, inst string) *job {
	t.Helper()
	var (
		j   *job
		err error
	)
	switch kind {
	case fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE:
		j, err = s.startCreateInstanceJob(&fleetgrpc.CreateInstanceRequest{Fleet: "alpha", Instance: inst, Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER}, false)
	case fleetgrpc.JobKind_JOB_KIND_CLONE_INSTANCE:
		j, err = s.startCloneInstanceJob(&fleetgrpc.CloneInstanceRequest{Fleet: "alpha", SourceInstance: "src", NewInstance: inst})
	case fleetgrpc.JobKind_JOB_KIND_REBUILD_INSTANCE:
		j, err = s.startRebuildInstanceJob(&fleetgrpc.RebuildInstanceRequest{Fleet: "alpha", Instance: inst})
	default:
		t.Fatalf("not a provisioning kind: %v", kind)
	}
	if err != nil {
		t.Fatalf("start %v: %v", kind, err)
	}
	return j
}

// seedProvisioningFleet writes fleet alpha with a running source instance (for
// clones) and a running "busy" instance (for rebuilds).
func seedProvisioningFleet(t *testing.T) {
	t.Helper()
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@x:a.git", Instances: []*fleet.Instance{
			{Name: "src", Status: fleet.StatusRunning, ContainerID: "c-src", Backend: fleet.BackendDevcontainer},
			{Name: "busy", Status: fleet.StatusRunning, ContainerID: "c-busy", Backend: fleet.BackendDevcontainer},
		}},
		"beta": {Name: "beta", Remote: "git@x:b.git"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func awaitDone(t *testing.T, j *job) *fleetgrpc.JobDone {
	t.Helper()
	select {
	case <-j.exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("job %s never exited", j.summary.GetJobId())
	}
	return j.outcome()
}

func destroyOne(t *testing.T, s *service, inst string) *job {
	t.Helper()
	j, err := s.startDestroyInstanceJob(&fleetgrpc.DestroyInstanceRequest{Fleet: "alpha", Instance: &inst})
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	return j
}

// TestDestroyCancelsInFlightProvisioning is destroy-cancels-create for each
// provisioning kind: the destroy marks the record deleting, cancels the job
// with errInstanceDestroyed, and waits for it to exit before removing the
// record, so the run never has its record pulled out from under it.
func TestDestroyCancelsInFlightProvisioning(t *testing.T) {
	stubHostServices(t, svcruntimetest.New("docker"))
	cases := []struct {
		kind fleetgrpc.JobKind
		inst string
	}{
		{fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "fresh"},
		{fleetgrpc.JobKind_JOB_KIND_CLONE_INSTANCE, "copy"},
		{fleetgrpc.JobKind_JOB_KIND_REBUILD_INSTANCE, "busy"},
	}
	for _, tc := range cases {
		t.Run(jobKindName(tc.kind), func(t *testing.T) {
			seedProvisioningFleet(t)
			stub := stubProvisioning(t, false)
			s := newService()

			pj := startProvisioning(t, s, tc.kind, tc.inst)
			stub.awaitEntered(t, "alpha/"+tc.inst)

			dj := destroyOne(t, s, tc.inst)
			r := stub.awaitStopped(t)
			if !errors.Is(r.cause, errInstanceDestroyed) {
				t.Fatalf("run cancelled with cause %v, want errInstanceDestroyed", r.cause)
			}
			if r.status != fleet.StatusDeleting {
				t.Fatalf("record was %q when the run stopped; the destroy must mark it deleting first and remove it only after the run exits", r.status)
			}

			if d := awaitDone(t, pj); d.GetSuccess() || !strings.Contains(d.GetError(), "instance destroyed") {
				t.Fatalf("provisioning job outcome = %v, want a failure naming the destroy", d)
			}
			if d := awaitDone(t, dj); !d.GetSuccess() || len(d.GetWarnings()) != 0 {
				t.Fatalf("destroy outcome = %v, want a clean success", d)
			}
			st, _ := state.Load()
			if findInstance(st, "alpha", tc.inst) != nil {
				t.Fatal("destroyed record still present")
			}
		})
	}
}

// TestDestroyProceedsPastAStuckJob: a run wedged in a backend call that takes
// no context cannot exit when cancelled; the destroy waits destroyCancelWait,
// then tears down anyway with a warning naming the job.
func TestDestroyProceedsPastAStuckJob(t *testing.T) {
	stubHostServices(t, svcruntimetest.New("docker"))
	seedProvisioningFleet(t)
	stub := stubProvisioning(t, true)
	orig := destroyCancelWait
	destroyCancelWait = 50 * time.Millisecond
	t.Cleanup(func() { destroyCancelWait = orig })
	s := newService()

	pj := startProvisioning(t, s, fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "fresh")
	stub.awaitEntered(t, "alpha/fresh")

	d := awaitDone(t, destroyOne(t, s, "fresh"))
	if !d.GetSuccess() || len(d.GetWarnings()) != 1 || !strings.Contains(d.GetWarnings()[0], "create_instance job for alpha/fresh still running") {
		t.Fatalf("destroy outcome = %v, want success with one stuck-job warning", d)
	}
	st, _ := state.Load()
	if findInstance(st, "alpha", "fresh") != nil {
		t.Fatal("record must be removed even though the job was stuck")
	}

	// The wedged call returns at last: the run sees its cancellation.
	close(stub.release)
	if r := stub.awaitStopped(t); !errors.Is(r.cause, errInstanceDestroyed) {
		t.Fatalf("late run's cause = %v, want errInstanceDestroyed", r.cause)
	}
	awaitDone(t, pj)
}

// TestDestroyCancelsOnlyItsTargets: an instance destroy leaves the fleet's
// other provisioning jobs (and other fleets') alone; a fleet destroy cancels
// every one in that fleet and none elsewhere.
func TestDestroyCancelsOnlyItsTargets(t *testing.T) {
	stubHostServices(t, svcruntimetest.New("docker"))
	seedProvisioningFleet(t)
	stub := stubProvisioning(t, false)
	s := newService()

	a1 := startProvisioning(t, s, fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "a1")
	stub.awaitEntered(t, "alpha/a1")
	a2 := startProvisioning(t, s, fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "a2")
	stub.awaitEntered(t, "alpha/a2")
	b1, err := s.startCreateInstanceJob(&fleetgrpc.CreateInstanceRequest{Fleet: "beta", Instance: "b1", Backend: fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER}, false)
	if err != nil {
		t.Fatalf("create beta/b1: %v", err)
	}
	stub.awaitEntered(t, "beta/b1")

	awaitDone(t, destroyOne(t, s, "a1"))
	if r := stub.awaitStopped(t); r.key != "alpha/a1" {
		t.Fatalf("instance destroy stopped %s, want only alpha/a1", r.key)
	}
	for _, j := range []*job{a2, b1} {
		if err := j.ctx.Err(); err != nil {
			t.Fatalf("%s/%s was cancelled by another instance's destroy", j.summary.GetFleet(), j.summary.GetInstance())
		}
	}

	fj, err := s.startDestroyInstanceJob(&fleetgrpc.DestroyInstanceRequest{Fleet: "alpha", DestroyFleet: true})
	if err != nil {
		t.Fatalf("destroy fleet: %v", err)
	}
	awaitDone(t, fj)
	if r := stub.awaitStopped(t); r.key != "alpha/a2" {
		t.Fatalf("fleet destroy stopped %s, want alpha/a2", r.key)
	}
	if b1.ctx.Err() != nil {
		t.Fatal("a fleet destroy cancelled another fleet's job")
	}

	close(stub.release)
	stub.awaitStopped(t)
	if d := awaitDone(t, b1); !d.GetSuccess() {
		t.Fatalf("untouched job failed: %v", d)
	}
	awaitDone(t, a1)
	awaitDone(t, a2)
}

// TestDestroyLeavesStartStopJobsAlone: only create, clone and rebuild are
// cancelled; a start or stop job of the target is not a provisioning race.
func TestDestroyLeavesStartStopJobsAlone(t *testing.T) {
	m := newJobManager()
	ctx := context.Background()
	start := m.start(ctx, fleetgrpc.JobKind_JOB_KIND_START_INSTANCE, "alpha", "i1", time.Now())
	stop := m.start(ctx, fleetgrpc.JobKind_JOB_KIND_STOP_INSTANCE, "alpha", "i1", time.Now())
	create := m.start(ctx, fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "alpha", "i1", time.Now())

	got := m.cancelProvisioning("alpha", "i1", errInstanceDestroyed)
	if len(got) != 1 || got[0] != create {
		t.Fatalf("cancelled %d jobs, want only the create", len(got))
	}
	if start.ctx.Err() != nil || stop.ctx.Err() != nil {
		t.Fatal("start/stop jobs must not be cancelled by a destroy")
	}
	if !errors.Is(context.Cause(create.ctx), errInstanceDestroyed) {
		t.Fatalf("create cause = %v", context.Cause(create.ctx))
	}
	if !m.hasProvisioningJob("alpha", "i1") || m.hasProvisioningJob("alpha", "i2") {
		t.Fatal("hasProvisioningJob must report the in-flight create, and only for its instance")
	}
}

// TestJobContextCauses: a job's context ends with errDaemonShutdown when the
// daemon's background context ends, with the given cause when cancelled on
// its own, and carries the parent's values.
func TestJobContextCauses(t *testing.T) {
	type key struct{}
	parent, cancelParent := context.WithCancel(context.WithValue(context.Background(), key{}, "v"))
	ctx, cancel := newJobContext(parent)
	defer cancel(nil)
	if ctx.Value(key{}) != "v" {
		t.Fatal("job context lost the parent's values")
	}
	cancelParent()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job context outlived the daemon's background context")
	}
	if !errors.Is(context.Cause(ctx), errDaemonShutdown) {
		t.Fatalf("cause = %v, want errDaemonShutdown", context.Cause(ctx))
	}

	ctx2, cancel2 := newJobContext(context.Background())
	cancel2(errInstanceDestroyed)
	if !errors.Is(context.Cause(ctx2), errInstanceDestroyed) {
		t.Fatalf("cause = %v, want errInstanceDestroyed", context.Cause(ctx2))
	}
}

// TestRunJobReleasesItsContext: once a job's work returns, its context ends
// and exited closes, so a waiting destroy moves on.
func TestRunJobReleasesItsContext(t *testing.T) {
	isolateFleetDir(t)
	s := newService()
	j := s.jobs.start(s.bgCtx, fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "alpha", "i1", time.Now())
	s.runJob(j, func() (*fleetgrpc.Instance, []string, error) { return nil, nil, nil })
	select {
	case <-j.exited:
	default:
		t.Fatal("exited not closed after runJob returned")
	}
	if j.ctx.Err() == nil {
		t.Fatal("job context still live after runJob returned")
	}
	if stuck := awaitJobs(context.Background(), []*job{j}, time.Millisecond); len(stuck) != 0 {
		t.Fatal("a finished job reported as still running")
	}
}
