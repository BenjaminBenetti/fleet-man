//go:build !nok8s

package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// fakeKubePool is the pool as kubePods sees it.
type fakeKubePool struct {
	mu         sync.Mutex
	tracks     [][]kubernetes.Tracked
	obs        map[string]kubernetes.Observation // by StatefulSet
	parked     []string
	parkErr    error
	forgot     []string
	onChange   func(clusterID, sts string)
	onRunEnded func(context.Context, kubernetes.RunEnd)
}

func (f *fakeKubePool) Track(items []kubernetes.Tracked) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tracks = append(f.tracks, items)
}

func (f *fakeKubePool) Observe(_, sts string) kubernetes.Observation {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o, ok := f.obs[sts]; ok {
		return o
	}
	return kubernetes.Observation{Live: backend.LiveStatusUnknown}
}

func (f *fakeKubePool) OnChange(fn func(string, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onChange = fn
}

func (f *fakeKubePool) OnRunEnded(fn func(context.Context, kubernetes.RunEnd)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRunEnded = fn
}

func (f *fakeKubePool) Park(_ context.Context, _, sts string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.parkErr != nil {
		return f.parkErr
	}
	f.parked = append(f.parked, sts)
	return nil
}

func (f *fakeKubePool) Forget(t kubernetes.PodTarget) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgot = append(f.forgot, t.StatefulSet)
}

// observe sets what the pool sees of sts and tells kubePods.
func (f *fakeKubePool) observe(sts string, o kubernetes.Observation) {
	f.mu.Lock()
	f.obs[sts] = o
	fn := f.onChange
	f.mu.Unlock()
	if fn != nil {
		fn("c1", sts)
	}
}

func (f *fakeKubePool) lastTrack() []kubernetes.Tracked {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tracks) == 0 {
		return nil
	}
	return f.tracks[len(f.tracks)-1]
}

const kubeTestKey = "abcdefgh"

func kubeInstance(name string, status fleet.InstanceStatus) *fleet.Instance {
	return &fleet.Instance{Name: name, Status: status, Backend: fleet.BackendKubernetes, KubeCluster: "c1", KubeKey: kubeTestKey, ContainerID: "k8s:c1/ns/x@g1"}
}

func kubeSTS(name string) string { return names.Instance("alpha", name, kubeTestKey) }

type kubePodsRig struct {
	t        *testing.T
	svc      *service
	pool     *fakeKubePool
	poolUsed chan struct{}

	mu        sync.Mutex
	restarted []string
}

func newKubePodsRig(t *testing.T, instances ...*fleet.Instance) *kubePodsRig {
	t.Helper()
	return newKubePodsRigWith(t, nil, instances...)
}

// newKubePodsRigWith is newKubePodsRig with configure run on kubePods before
// it starts.
func newKubePodsRigWith(t *testing.T, configure func(*kubePods), instances ...*fleet.Instance) *kubePodsRig {
	t.Helper()
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: instances},
	}}); err != nil {
		t.Fatal(err)
	}
	r := &kubePodsRig{t: t, svc: newService(), pool: &fakeKubePool{obs: map[string]kubernetes.Observation{}}, poolUsed: make(chan struct{}, 1)}
	r.svc.kubePods.pool = func() kubePodPool {
		select {
		case r.poolUsed <- struct{}{}:
		default:
		}
		return r.pool
	}
	r.svc.kubePods.targetFor = func(inst *fleet.Instance) *kubernetes.PodTarget {
		return &kubernetes.PodTarget{Cluster: inst.KubeCluster, StatefulSet: names.Instance("alpha", inst.Name, inst.KubeKey)}
	}
	r.svc.kubePods.postRestart = func(fleetName string, inst *fleet.Instance, _ string) {
		r.mu.Lock()
		r.restarted = append(r.restarted, fleetName+"/"+inst.Name)
		r.mu.Unlock()
	}
	if configure != nil {
		configure(r.svc.kubePods)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.svc.hub.run(ctx)
	go r.svc.kubePods.run(ctx)
	loadAndPush(r.svc.hub)
	return r
}

func (r *kubePodsRig) restarts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.restarted...)
}

func (r *kubePodsRig) runtime(instance string) *fleetgrpc.InstanceRuntime {
	got := make(chan *fleetgrpc.InstanceRuntime, 1)
	r.svc.hub.post(func(h *hub) {
		if rt := h.runtime[runtimeKey("alpha", instance)]; rt != nil {
			got <- proto.Clone(rt).(*fleetgrpc.InstanceRuntime)
			return
		}
		got <- nil
	})
	return <-got
}

func (r *kubePodsRig) eventually(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *kubePodsRig) waitTracked() []kubernetes.Tracked {
	r.t.Helper()
	r.eventually("a Track", func() bool { return r.pool.lastTrack() != nil })
	return r.pool.lastTrack()
}

func TestKubePodsTracksTheKubernetesInstances(t *testing.T) {
	devc := &fleet.Instance{Name: "local", Status: fleet.StatusRunning, Backend: fleet.BackendDevcontainer, ContainerID: "abc"}
	r := newKubePodsRig(t, kubeInstance("one", fleet.StatusRunning), kubeInstance("two", fleet.StatusStopped), devc)
	items := r.waitTracked()
	if len(items) != 2 {
		t.Fatalf("tracked %+v, want the two Kubernetes instances", items)
	}
	byName := map[string]kubernetes.Tracked{}
	for _, it := range items {
		byName[it.StatefulSet] = it
	}
	one, two := byName[kubeSTS("one")], byName[kubeSTS("two")]
	if one.Cluster != "c1" || one.Target == nil || one.Target.Fleet != "alpha" || one.Target.Instance != "one" || one.Target.Priority != kubernetes.PriorityBackground {
		t.Fatalf("the running instance: %+v target %+v", one, one.Target)
	}
	if two.Cluster != "c1" || two.Target != nil {
		t.Fatalf("the stopped instance: %+v, want watched only", two)
	}

	r.pool.observe(kubeSTS("two"), kubernetes.Observation{Live: backend.LiveStatusStopped, Restart: &backend.Restart{ID: "u2/0", Cause: backend.RestartNodeLost}})
	r.eventually("the stopped instance's runtime", func() bool { return r.runtime("two").GetKube() != nil })

	// Destroyed: forgotten, no longer tracked, and nothing of its pod left
	// on the name for whatever instance takes it next.
	if err := state.Update(func(st *state.State) error {
		st.Fleets["alpha"].Instances = st.Fleets["alpha"].Instances[:1]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loadAndPush(r.svc.hub)
	r.eventually("the destroyed instance forgotten", func() bool {
		r.pool.mu.Lock()
		defer r.pool.mu.Unlock()
		return len(r.pool.forgot) == 1 && r.pool.forgot[0] == kubeSTS("two")
	})
	if items := r.pool.lastTrack(); len(items) != 1 || items[0].StatefulSet != kubeSTS("one") {
		t.Fatalf("tracked after the destroy: %+v", items)
	}
	r.eventually("the destroyed instance's runtime cleared", func() bool {
		rt := r.runtime("two")
		return rt.GetKube() == nil && rt.GetLiveStatus() == fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_UNSPECIFIED
	})
}

func TestKubePodsLeavesThePoolAloneWithoutKubernetesInstances(t *testing.T) {
	r := newKubePodsRig(t, &fleet.Instance{Name: "local", Status: fleet.StatusRunning, Backend: fleet.BackendDevcontainer, ContainerID: "abc"})
	time.Sleep(100 * time.Millisecond)
	select {
	case <-r.poolUsed:
		t.Fatal("the pool was used with no Kubernetes instance")
	default:
	}
}

func TestKubePodsPriorities(t *testing.T) {
	f := &fleet.Fleet{}
	inst := &fleet.Instance{}
	if p := kubePriority(false, false, f, inst); p != kubernetes.PriorityBackground {
		t.Fatalf("plain: %v", p)
	}
	if p := kubePriority(true, false, f, inst); p != kubernetes.PriorityInteractive {
		t.Fatalf("a TUI watches: %v", p)
	}
	if p := kubePriority(false, true, f, inst); p != kubernetes.PriorityState {
		t.Fatalf("the microphone is on: %v", p)
	}
	if p := kubePriority(false, false, &fleet.Fleet{Settings: fleet.FleetSettings{FleetMCP: true}}, inst); p != kubernetes.PriorityState {
		t.Fatalf("Fleet MCP: %v", p)
	}
	if p := kubePriority(false, false, f, &fleet.Instance{Automated: true}); p != kubernetes.PriorityState {
		t.Fatalf("an automation run: %v", p)
	}

	// A TUI attaching raises every running instance's priority.
	r := newKubePodsRigWith(t, func(k *kubePods) { k.priorityCheck = 10 * time.Millisecond }, kubeInstance("one", fleet.StatusRunning))
	r.waitTracked()
	r.svc.hub.runtimeWanted.Store(true)
	r.eventually("the interactive re-track", func() bool {
		items := r.pool.lastTrack()
		return len(items) == 1 && items[0].Target != nil && items[0].Target.Priority == kubernetes.PriorityInteractive
	})
}

func TestKubePodsActsOnARestart(t *testing.T) {
	r := newKubePodsRig(t, kubeInstance("one", fleet.StatusRunning))
	r.waitTracked()
	sts := kubeSTS("one")
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	// The cluster restarted the container (out of memory); it is not up
	// again yet: the notice shows, the post-start work waits.
	r.pool.observe(sts, kubernetes.Observation{Live: backend.LiveStatusUnknown, Reason: "ContainerCreating", Node: "n1",
		Restart: &backend.Restart{ID: "u1/1", Cause: backend.RestartOOMKilled, At: at}, SessionsEnded: true})
	r.eventually("the notice", func() bool {
		k := r.runtime("one").GetKube()
		return k.GetRestartCause() == string(backend.RestartOOMKilled) && k.GetSessionsEnded() && k.GetRestartedAt().AsTime().Equal(at) &&
			k.GetPodReason() == "ContainerCreating" && k.GetNode() == "n1"
	})
	if got := r.restarts(); len(got) != 0 {
		t.Fatalf("post-start ran before the pod did: %v", got)
	}

	// Up again: the post-start work runs, once.
	r.pool.observe(sts, kubernetes.Observation{Live: backend.LiveStatusRunning, Agent: kubernetes.AgentConnected,
		Restart: &backend.Restart{ID: "u1/1", Cause: backend.RestartOOMKilled, At: at}, SessionsEnded: true})
	r.eventually("the post-start re-run", func() bool { return len(r.restarts()) == 1 })
	r.eventually("the live runtime", func() bool {
		rt := r.runtime("one")
		return rt.GetKube().GetAgent() == fleetgrpc.KubeAgentState_KUBE_AGENT_STATE_CONNECTED && rt.GetLiveStatus() == fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_RUNNING
	})
	r.pool.observe(sts, kubernetes.Observation{Live: backend.LiveStatusRunning, Restart: &backend.Restart{ID: "u1/1", Cause: backend.RestartOOMKilled, At: at}})
	time.Sleep(50 * time.Millisecond)
	if got := r.restarts(); len(got) != 1 {
		t.Fatalf("post-start ran %d times for one restart", len(got))
	}

	// An attach clears the notice.
	r.svc.kubePods.attached("alpha", "one")
	r.eventually("the cleared notice", func() bool { return r.runtime("one").GetKube().GetRestartCause() == "" })

	// The next restart is another one.
	r.pool.observe(sts, kubernetes.Observation{Live: backend.LiveStatusRunning, Restart: &backend.Restart{ID: "u2/0", Cause: backend.RestartNodeDrained, At: at}})
	r.eventually("the second restart", func() bool {
		k := r.runtime("one").GetKube()
		return len(r.restarts()) == 2 && k.GetRestartCause() == string(backend.RestartNodeDrained) && k.GetRestartHint() == kubernetes.RestartHint(backend.RestartNodeDrained)
	})
}

func TestKubePodsLeavesFleetsOwnStartsAlone(t *testing.T) {
	r := newKubePodsRig(t, kubeInstance("one", fleet.StatusStarting))
	r.waitTracked()
	sts := kubeSTS("one")
	r.pool.observe(sts, kubernetes.Observation{Live: backend.LiveStatusRunning, Restart: &backend.Restart{ID: "u2/0", Cause: backend.RestartPodDeleted}})
	r.eventually("the observation", func() bool { return r.runtime("one") != nil })
	// The start finishes; the same restart is still the last one.
	if err := state.Update(func(st *state.State) error {
		st.Fleets["alpha"].Instances[0].Status = fleet.StatusRunning
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loadAndPush(r.svc.hub)
	r.eventually("the running record", func() bool {
		items := r.pool.lastTrack()
		return len(items) == 1 && items[0].Target != nil
	})
	r.pool.observe(sts, kubernetes.Observation{Live: backend.LiveStatusRunning, Restart: &backend.Restart{ID: "u2/0", Cause: backend.RestartPodDeleted}})
	time.Sleep(50 * time.Millisecond)
	if got := r.restarts(); len(got) != 0 {
		t.Fatalf("post-start re-run for fleet's own start: %v", got)
	}
	if c := r.runtime("one").GetKube().GetRestartCause(); c != "" {
		t.Fatalf("a notice for fleet's own start: %q", c)
	}
}

func TestKubePodsParksACrashLoop(t *testing.T) {
	r := newKubePodsRig(t, kubeInstance("one", fleet.StatusRunning))
	r.waitTracked()
	sts := kubeSTS("one")
	loop := &kubernetes.CrashLoop{OOM: true, Restarts: 3, MemoryLimit: "1Gi"}
	r.pool.observe(sts, kubernetes.Observation{Live: backend.LiveStatusUnknown, CrashLoop: loop})
	r.eventually("the parked record", func() bool {
		st, err := state.Load()
		return err == nil && st.Fleets["alpha"].Instances[0].Status == fleet.StatusStopped
	})
	r.pool.mu.Lock()
	parked := append([]string(nil), r.pool.parked...)
	r.pool.mu.Unlock()
	if len(parked) != 1 || parked[0] != sts {
		t.Fatalf("parked %v", parked)
	}
	warn, err := os.ReadFile(state.WarnPath("alpha", "one"))
	if err != nil || !strings.HasPrefix(string(warn), kubernetes.CodeOOMKilled+": ") {
		t.Fatalf("warning %q %v", warn, err)
	}
	// The record says so too, for every client (the recovery dialog reads
	// the code off it); the next start clears it.
	if st, err := state.Load(); err != nil || !strings.HasPrefix(st.Fleets["alpha"].Instances[0].Error, kubernetes.CodeOOMKilled+": ") {
		t.Fatalf("the parked record's error: %+v %v", st.Fleets["alpha"].Instances[0], err)
	}
	// Stopped now: the loop is not parked twice.
	r.pool.observe(sts, kubernetes.Observation{Live: backend.LiveStatusUnknown, CrashLoop: loop})
	time.Sleep(50 * time.Millisecond)
	r.pool.mu.Lock()
	defer r.pool.mu.Unlock()
	if len(r.pool.parked) != 1 {
		t.Fatalf("parked %v", r.pool.parked)
	}
}

func TestKubePodsRetriesAFailedPark(t *testing.T) {
	r := newKubePodsRigWith(t, func(k *kubePods) { k.parkRetry = 50 * time.Millisecond }, kubeInstance("one", fleet.StatusRunning))
	r.waitTracked()
	sts := kubeSTS("one")
	r.pool.mu.Lock()
	r.pool.parkErr = errors.New("the API server does not answer")
	r.pool.mu.Unlock()
	loop := &kubernetes.CrashLoop{Reason: "Error", ExitCode: 1, Restarts: 3}
	r.pool.observe(sts, kubernetes.Observation{Live: backend.LiveStatusUnknown, CrashLoop: loop})
	time.Sleep(20 * time.Millisecond)
	// Right away again: within the retry wait, nothing.
	r.pool.observe(sts, kubernetes.Observation{Live: backend.LiveStatusUnknown, CrashLoop: loop})
	if st, _ := state.Load(); st.Fleets["alpha"].Instances[0].Status != fleet.StatusRunning {
		t.Fatal("a failed park recorded the instance stopped")
	}
	r.pool.mu.Lock()
	r.pool.parkErr = nil
	r.pool.mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	r.pool.observe(sts, kubernetes.Observation{Live: backend.LiveStatusUnknown, CrashLoop: loop})
	r.eventually("the parked record", func() bool {
		st, err := state.Load()
		return err == nil && st.Fleets["alpha"].Instances[0].Status == fleet.StatusStopped
	})
}

func TestHubTellsStateListenersOncePerChange(t *testing.T) {
	isolateFleetDir(t)
	h := newHub()
	var mu sync.Mutex
	var told []*state.State
	h.stateListeners = append(h.stateListeners, func(st *state.State) {
		mu.Lock()
		told = append(told, st)
		mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.run(ctx)
	count := func() int {
		done := make(chan int)
		h.post(func(*hub) {
			mu.Lock()
			done <- len(told)
			mu.Unlock()
		})
		return <-done
	}
	loadAndPush(h)
	loadAndPush(h)
	if n := count(); n != 1 {
		t.Fatalf("told %d times for one state", n)
	}
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha"}}}); err != nil {
		t.Fatal(err)
	}
	loadAndPush(h)
	if n := count(); n != 2 {
		t.Fatalf("told %d times for two states", n)
	}
}

func TestKubePodsNoticeClearsOnAnAttach(t *testing.T) {
	r := newKubePodsRig(t, kubeInstance("one", fleet.StatusRunning))
	r.waitTracked()
	sts := kubeSTS("one")
	r.pool.observe(sts, kubernetes.Observation{Live: backend.LiveStatusRunning, Restart: &backend.Restart{ID: "u2/0", Cause: backend.RestartEvicted}})
	r.eventually("the notice", func() bool { return r.runtime("one").GetKube().GetRestartCause() == string(backend.RestartEvicted) })

	// A one-shot through the resolved argv is no attach.
	if _, err := r.svc.ResolveExecCommand(context.Background(), &fleetgrpc.ResolveExecCommandRequest{Fleet: "alpha", Instance: "one", Argv: []string{"tmux", "ls"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if c := r.runtime("one").GetKube().GetRestartCause(); c == "" {
		t.Fatal("a one-shot cleared the notice")
	}
	// A pane attaching is.
	if _, err := r.svc.ResolveExecCommand(context.Background(), &fleetgrpc.ResolveExecCommandRequest{Fleet: "alpha", Instance: "one", Argv: []string{"bash"}, Attach: true}); err != nil {
		t.Fatal(err)
	}
	r.eventually("the cleared notice", func() bool { return r.runtime("one").GetKube().GetRestartCause() == "" })
}

// The backend handle of a Kubernetes instance names its pod from fleetd's
// record of it: without one the instance is only watched. Post-start work the
// handle cannot do becomes the instance's warning.
func TestKubeHandlesNameTheirRecordedPod(t *testing.T) {
	isolateFleetDir(t)
	inst := kubeInstance("one", fleet.StatusRunning)
	inst.WorkspaceDir = filepath.Join(state.WorkspacesDir(), "alpha", "one", "alpha")
	if tgt := kubeTargetFor(inst); tgt != nil {
		t.Fatalf("target %+v of an instance fleetd has no record of", tgt)
	}
	rec := kubernetes.InstanceRecord{Cluster: "c1", Namespace: "ns", Key: kubeTestKey, Fleet: "alpha", Instance: "one",
		StatefulSet: kubeSTS("one"), StatefulSetUID: "uid-1", Gen: 1, SpecHash: "sha256:abc", ImageDigests: []string{"sha256:" + strings.Repeat("1", 64)}}
	if err := kubernetes.SaveInstanceRecord(rec); err != nil {
		t.Fatal(err)
	}
	tgt := kubeTargetFor(inst)
	if tgt == nil || tgt.Cluster != "c1" || tgt.StatefulSet != kubeSTS("one") || tgt.Expect.SpecHash != "sha256:abc" ||
		string(tgt.Expect.StatefulSetUID) != "uid-1" || tgt.SpecSecret != names.SpecSecret(kubeSTS("one"), 1) {
		t.Fatalf("target %+v", tgt)
	}
	// The connection is not one the pool knows: the hooks cannot run, and
	// the instance's banner says why.
	kubePostRestart("alpha", inst, "")
	if data, err := os.ReadFile(state.WarnPath("alpha", "one")); err != nil || !strings.Contains(string(data), "kubernetes:") {
		t.Fatalf("warning %q, %v", data, err)
	}
}

// A terminal attached over the Exec stream (a remote TUI's pane, `fleet
// shell` through a remote daemon) clears the notice; a one-shot does not.
func TestKubePodsNoticeClearsOnATTYExec(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{kubeInstance("one", fleet.StatusRunning)}},
	}}); err != nil {
		t.Fatal(err)
	}
	stubExecCommand(t)
	// The kubernetes backend is a StreamExecer, so the Exec RPC opens the
	// command through it, not a host command. Stand in an in-memory stream
	// that exits at once (no cluster here); the notice-clearing this test is
	// about runs when a TTY attaches.
	origOpen := openExecStream
	openExecStream = func(_ context.Context, _ *fleet.Instance, _ *fleetgrpc.ExecStart) (backend.ExecStream, error) {
		s := newFakeExecStream()
		s.exit(0, nil)
		return s, nil
	}
	t.Cleanup(func() { openExecStream = origOpen })
	svc, client, cleanup := newTestServer(t)
	defer cleanup()
	pool := &fakeKubePool{obs: map[string]kubernetes.Observation{}}
	svc.kubePods.pool = func() kubePodPool { return pool }
	svc.kubePods.targetFor = func(*fleet.Instance) *kubernetes.PodTarget { return nil }
	svc.kubePods.postRestart = func(string, *fleet.Instance, string) {}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.kubePods.run(ctx)
	loadAndPush(svc.hub)
	r := &kubePodsRig{t: t, svc: svc, pool: pool}
	r.waitTracked()
	pool.observe(kubeSTS("one"), kubernetes.Observation{Live: backend.LiveStatusRunning, Restart: &backend.Restart{ID: "u2/0", Cause: backend.RestartNodeLost}})
	r.eventually("the notice", func() bool { return r.runtime("one").GetKube().GetRestartCause() == string(backend.RestartNodeLost) })

	exec := func(tty bool) {
		t.Helper()
		sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
		defer scancel()
		stream, err := client.Exec(sctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(execStartFrame("alpha", "one", []string{"true"}, tty, nil)); err != nil {
			t.Fatal(err)
		}
		_ = stream.CloseSend()
		drainExec(t, stream)
	}
	exec(false)
	time.Sleep(50 * time.Millisecond)
	if c := r.runtime("one").GetKube().GetRestartCause(); c == "" {
		t.Fatal("a one-shot exec cleared the notice")
	}
	exec(true)
	r.eventually("the cleared notice", func() bool { return r.runtime("one").GetKube().GetRestartCause() == "" })
}
