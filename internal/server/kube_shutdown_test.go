//go:build !nok8s

package server

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// fakeShutdownPool stands in for the pool in the serve loop's shutdown.
type fakeShutdownPool struct {
	mu    sync.Mutex
	calls []string
	// sessionsLeft is the time CloseSessions was given.
	sessionsLeft time.Duration
	// deaf: CloseSessions waits for its deadline, as for agents that never
	// answer the Close.
	deaf bool
}

func (p *fakeShutdownPool) record(call string) {
	p.mu.Lock()
	p.calls = append(p.calls, call)
	p.mu.Unlock()
}

func (p *fakeShutdownPool) CloseSessions(ctx context.Context) {
	dl, _ := ctx.Deadline()
	p.mu.Lock()
	p.calls = append(p.calls, "sessions")
	p.sessionsLeft = time.Until(dl)
	deaf := p.deaf
	p.mu.Unlock()
	if deaf {
		<-ctx.Done()
	}
}

func (p *fakeShutdownPool) ClosePortForwards() { p.record("forwards") }

func (p *fakeShutdownPool) Close() { p.record("close") }

func (p *fakeShutdownPool) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

// servePool is the stand-in the serve loop startServe last ran ends.
var servePool *fakeShutdownPool

func stubServeShutdownPool(t *testing.T) {
	t.Helper()
	orig := kubeShutdownPool
	p := &fakeShutdownPool{}
	servePool = p
	kubeShutdownPool = func() kubeShutdowner { return p }
	t.Cleanup(func() { kubeShutdownPool = orig })
}

// The serve loop ends the daemon's own pool.
func TestKubeShutdownEndsTheDaemonsPool(t *testing.T) {
	if p, ok := defaultKubeShutdownPool().(*kubernetes.Pool); !ok || p != kubernetes.DefaultPool() {
		t.Fatalf("the shutdown ends %v, not the daemon's pool", defaultKubeShutdownPool())
	}
}

// Once the shutdown begins, the daemon-host shim's RPCs are refused as from a
// daemon that is away (Unavailable), so a pane resuming meanwhile goes to the
// kubeconfig instead; before it they reach their handlers.
func TestShutdownRefusesTheShimsRPCs(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	gs := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gs, svc)
	client := dialBuf(t, gs)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bad := &fleetgrpc.ExecIn{Msg: &fleetgrpc.ExecIn_Start{Start: &fleetgrpc.ExecStart{}}}

	reached := podExecFirstErr(t, client, ctx, bad)
	_, credErr := client.IssueKubeExecCredential(ctx, &fleetgrpc.IssueKubeExecCredentialRequest{})
	if reached == codes.Unavailable || status.Code(credErr) == codes.Unavailable {
		t.Fatalf("before the shutdown: PodExec %v, IssueKubeExecCredential %v", reached, credErr)
	}

	svc.beginShutdown()
	if code := podExecFirstErr(t, client, ctx, bad); code != codes.Unavailable {
		t.Errorf("PodExec during the shutdown: %v, want Unavailable", code)
	}
	if _, err := client.IssueKubeExecCredential(ctx, &fleetgrpc.IssueKubeExecCredentialRequest{ClusterId: "mk-abc"}); status.Code(err) != codes.Unavailable {
		t.Errorf("IssueKubeExecCredential during the shutdown: %v, want Unavailable", err)
	}
}

// TestServeShutdownClosesTheAgentSessions is the open-session case: after
// the shared-state flush, the pods' sessions are told to close and given
// their 2s, even when no agent answers; then the pool ends. The daemon is
// gone within the budget.
func TestServeShutdownClosesTheAgentSessions(t *testing.T) {
	exited := stubHardExit(t)
	stubShareFlush(t, func(context.Context) error {
		servePool.record("flush")
		return nil
	})
	client, served := startServe(t)
	pool := servePool
	pool.mu.Lock()
	pool.deaf = true
	pool.mu.Unlock()

	start := time.Now()
	shutdownAndWait(t, client, served)
	took := time.Since(start)

	if got, want := pool.recorded(), []string{"flush", "sessions", "forwards", "close"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("shutdown steps %v, want %v", got, want)
	}
	pool.mu.Lock()
	left := pool.sessionsLeft
	pool.mu.Unlock()
	if left > sessionCloseLimit || left < sessionCloseLimit-time.Second {
		t.Errorf("the sessions had %v to close, want %v", left, sessionCloseLimit)
	}
	if took < sessionCloseLimit || took > shutdownBudget+drainTimeout+3*time.Second {
		t.Errorf("shutdown took %v", took)
	}
	select {
	case <-exited:
		t.Fatal("the shutdown hit its hard limit")
	default:
	}
}

// openPodExec is a pod command that runs until it is closed.
type openPodExec struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
	out     *io.PipeReader
	outW    *io.PipeWriter
	errR    *io.PipeReader
	errW    *io.PipeWriter
}

func newOpenPodExec() *openPodExec {
	e := &openPodExec{started: make(chan struct{}), closed: make(chan struct{})}
	e.out, e.outW = io.Pipe()
	e.errR, e.errW = io.Pipe()
	return e
}

func (e *openPodExec) OpenPodExec(context.Context, kubernetes.PodCommand) (backend.ExecStream, error) {
	close(e.started)
	return e, nil
}

func (e *openPodExec) Stdin() io.WriteCloser       { return nopWriteCloser{} }
func (e *openPodExec) Stdout() io.Reader           { return e.out }
func (e *openPodExec) Stderr() io.Reader           { return e.errR }
func (e *openPodExec) Resize(uint16, uint16) error { return nil }
func (e *openPodExec) Wait() (int, error) {
	<-e.closed
	return -1, nil
}

func (e *openPodExec) Close() error {
	e.once.Do(func() {
		_ = e.outW.Close()
		_ = e.errW.Close()
		close(e.closed)
	})
	return nil
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// TestServeShutdownEndsAnOpenPodExec is the open-PodExec case: a shim
// command running through PodExec when the shutdown begins is ended (its
// command closed, not left running in the pod) and so is its stream, without
// waiting out the drain.
func TestServeShutdownEndsAnOpenPodExec(t *testing.T) {
	setDrainTimeout(t, time.Minute)
	key := names.InstanceKey("daemon12", "alpha", "web", time.Unix(1700000000, 0).UTC())
	sts := names.Instance("alpha", "web", key)
	client, served := startServeSeeded(t, func() {
		if err := state.Update(func(st *state.State) error {
			f := st.GetOrCreateFleet("alpha", "https://example.invalid/a.git")
			return f.AddInstance(&fleet.Instance{Name: "web", Status: fleet.StatusRunning, Backend: fleet.BackendKubernetes, KubeCluster: "mk-abc", KubeKey: key})
		}); err != nil {
			t.Fatal(err)
		}
	})
	cmd := newOpenPodExec()
	orig := podExecerFor
	podExecerFor = func(*fleet.Instance) (podExecer, bool) { return cmd, true }
	t.Cleanup(func() { podExecerFor = orig })

	stream, err := client.PodExec(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&fleetgrpc.ExecIn{Msg: &fleetgrpc.ExecIn_PodStart{PodStart: &fleetgrpc.PodExecStart{
		ClusterId: "mk-abc", Pod: names.Pod(sts), Argv: []string{"sleep", "infinity"},
	}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cmd.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the pod command never started")
	}

	start := time.Now()
	shutdownAndWait(t, client, served)
	if took := time.Since(start); took > shutdownBudget {
		t.Errorf("shutdown took %v with a PodExec open", took)
	}
	select {
	case <-cmd.closed:
	default:
		t.Fatal("the pod command was left running")
	}
	// The stream has ended: with the command's exit, as when the shim's
	// own stream drops, or Unavailable when that could not be sent.
	exited := false
	for {
		out, err := stream.Recv()
		if err == nil {
			exited = exited || out.GetExit() != nil
			continue
		}
		if !(exited && errors.Is(err, io.EOF)) && status.Code(err) != codes.Unavailable {
			t.Fatalf("the PodExec stream ended with %v (exit sent: %v)", err, exited)
		}
		break
	}
}

// The jobs the shutdown checkpoints are the Kubernetes records' provisioning
// jobs: they are waited for (they stop at their next step), within the
// step's deadline; other backends' jobs, and start/stop jobs, are not.
func TestCheckpointKubeJobs(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Update(func(st *state.State) error {
		f := st.GetOrCreateFleet("alpha", "https://example.invalid/a.git")
		for _, inst := range []*fleet.Instance{
			{Name: "pod", Status: fleet.StatusCreating, Backend: fleet.BackendKubernetes, KubeCluster: "mk-abc", KubeKey: "aaaaaaaa"},
			{Name: "stuck", Status: fleet.StatusRebuilding, Backend: fleet.BackendKubernetes, KubeCluster: "mk-abc", KubeKey: "bbbbbbbb"},
			{Name: "docker", Status: fleet.StatusCreating, Backend: fleet.BackendDevcontainer},
		} {
			if err := f.AddInstance(inst); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := newService()
	bg, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	run := func(kind fleetgrpc.JobKind, inst string, work func(ctx context.Context)) *job {
		j := svc.jobs.start(bg, kind, "alpha", inst, time.Now())
		go svc.runJob(j, func() (*fleetgrpc.Instance, []string, error) {
			work(j.ctx)
			return nil, nil, errors.New("cancelled")
		})
		return j
	}
	release := make(chan struct{})
	defer close(release)
	// Winds down a moment after its cancellation, as a create does at its
	// next step.
	create := run(fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "pod", func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
	})
	docker := run(fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "docker", func(context.Context) { <-release })
	start := run(fleetgrpc.JobKind_JOB_KIND_START_INSTANCE, "pod", func(context.Context) { <-release })

	got := svc.kubeProvisioningJobs()
	if len(got) != 1 || got[0] != create {
		t.Fatalf("jobs to checkpoint: %v", got)
	}
	shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	began := time.Now()
	svc.checkpointKubeJobs(ctx)
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("checkpoint took %v: it waited for jobs that were not Kubernetes provisioning", took)
	}
	select {
	case <-create.exited:
	default:
		t.Fatal("the checkpoint returned before the create stopped")
	}
	for _, j := range []*job{docker, start} {
		select {
		case <-j.exited:
			t.Fatal("a job that ignores its context ended")
		default:
		}
	}

	// A Kubernetes job that never stops holds the step only until its
	// deadline.
	stuck := run(fleetgrpc.JobKind_JOB_KIND_REBUILD_INSTANCE, "stuck", func(context.Context) { <-release })
	_ = stuck
	short, cancelShort := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelShort()
	began = time.Now()
	svc.checkpointKubeJobs(short)
	if took := time.Since(began); took < 150*time.Millisecond || took > 2*time.Second {
		t.Fatalf("checkpoint with a stuck job took %v, want its 200ms deadline", took)
	}
}

// A daemon served again in the same process starts with a live pool: the
// last one's shutdown ended the pool it had.
func TestServeAgainGetsALivePool(t *testing.T) {
	client, served := startServe(t)
	// This daemon's shutdown ends the real pool.
	kubeShutdownPool = defaultKubeShutdownPool
	shutdownAndWait(t, client, served)
	if _, err := kubernetes.DefaultPool().Cluster("nope-aaaaaa"); errors.Is(err, kubernetes.ErrClusterNotFound) {
		t.Fatal("the shutdown left the pool up")
	}

	client, served = startServe(t)
	if _, err := kubernetes.DefaultPool().Cluster("nope-aaaaaa"); !errors.Is(err, kubernetes.ErrClusterNotFound) {
		t.Fatalf("the next daemon's pool: %v, want a live one", err)
	}
	shutdownAndWait(t, client, served)
}

// The pool ends only once the Kubernetes jobs reached their checkpoint (or
// the budget ran out): a cancelled create still scales its pod down through
// it while it winds down.
func TestKubeShutdownEndsThePoolAfterTheCheckpoint(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Update(func(st *state.State) error {
		return st.GetOrCreateFleet("alpha", "https://example.invalid/a.git").AddInstance(
			&fleet.Instance{Name: "pod", Status: fleet.StatusCreating, Backend: fleet.BackendKubernetes, KubeCluster: "mk-abc", KubeKey: "aaaaaaaa"})
	}); err != nil {
		t.Fatal(err)
	}
	stubServeShutdownPool(t)
	pool := servePool
	svc := newService()
	bg, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	j := svc.jobs.start(bg, fleetgrpc.JobKind_JOB_KIND_CREATE_INSTANCE, "alpha", "pod", time.Now())
	go svc.runJob(j, func() (*fleetgrpc.Instance, []string, error) {
		<-j.ctx.Done()
		time.Sleep(200 * time.Millisecond)
		pool.record("job stopped")
		return nil, nil, errors.New("cancelled")
	})
	shutdown()
	runShutdownSteps(5*time.Second, svc.kubeShutdownSteps())
	if got, want := pool.recorded(), []string{"sessions", "forwards", "job stopped", "close"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("shutdown %v, want %v", got, want)
	}
}

// A quiet PodExec (the shim's argv for ExecCommandQuiet: polls, probes) is
// left out of fleet.log; any other is logged once it ends. In a process of
// its own, so the process-wide log is this test's.
func TestPodExecQuietStaysOutOfTheLog(t *testing.T) {
	if os.Getenv("FLEET_TEST_POD_EXEC_LOG") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPodExecQuietStaysOutOfTheLog$", "-test.count=1")
		cmd.Env = append(os.Environ(), "FLEET_TEST_POD_EXEC_LOG=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("log subprocess: %v\n%s", err, out)
		}
		return
	}
	key := names.InstanceKey("daemon12", "alpha", "web", time.Unix(1700000000, 0).UTC())
	sts := names.Instance("alpha", "web", key)
	client, served := startServeSeeded(t, func() {
		if err := state.Update(func(st *state.State) error {
			f := st.GetOrCreateFleet("alpha", "https://example.invalid/a.git")
			return f.AddInstance(&fleet.Instance{Name: "web", Status: fleet.StatusRunning, Backend: fleet.BackendKubernetes, KubeCluster: "mk-abc", KubeKey: key})
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Cleanup(func() { shutdownAndWait(t, client, served) })
	orig := podExecerFor
	podExecerFor = func(*fleet.Instance) (podExecer, bool) {
		cmd := newOpenPodExec()
		_ = cmd.Close() // the command ends at once
		return cmd, true
	}
	t.Cleanup(func() { podExecerFor = orig })

	run := func(marker string, quiet bool) {
		t.Helper()
		stream, err := client.PodExec(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&fleetgrpc.ExecIn{Msg: &fleetgrpc.ExecIn_PodStart{PodStart: &fleetgrpc.PodExecStart{
			ClusterId: "mk-abc", Pod: names.Pod(sts), Argv: []string{"echo", marker}, Quiet: quiet,
		}}}); err != nil {
			t.Fatal(err)
		}
		_ = stream.CloseSend()
		for {
			if _, err := stream.Recv(); err != nil {
				break
			}
		}
	}
	run("itest-quiet-marker", true)
	run("itest-loud-marker", false)
	logged := func() string {
		data, _ := os.ReadFile(flog.Path())
		return string(data)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logged(), "itest-loud-marker") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if log := logged(); !strings.Contains(log, "itest-loud-marker") {
		t.Fatalf("the PodExec was not logged:\n%s", log)
	} else if strings.Contains(log, "itest-quiet-marker") {
		t.Fatalf("a quiet PodExec was logged:\n%s", log)
	}
}
