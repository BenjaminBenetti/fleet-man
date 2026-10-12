//go:build !nok8s && linux

package kubernetes

import (
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/binstore"
	"github.com/BenjaminBenetti/fleet-man/internal/execas"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// The tests below run the supervisor over the harness's local pods with the
// pool's real informers on the fake clientset: Track names instances, and
// everything else (connecting, reconnecting, refusing, re-attaching) happens
// on the informers' events, never on an EnsureAgent of the test's.

// eventually waits for cond, failing with what after 15s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) track(targets ...PodTarget) {
	items := make([]Tracked, len(targets))
	for i := range targets {
		t := targets[i]
		items[i] = Tracked{Cluster: t.Cluster, StatefulSet: t.StatefulSet, Target: &t}
	}
	h.pool.Track(items)
}

func (h *harness) session(t *testing.T, target PodTarget, not *AgentSession) *AgentSession {
	t.Helper()
	var s *AgentSession
	eventually(t, "the supervisor's session", func() bool {
		s = h.pool.Session(target)
		return s != nil && s != not
	})
	return s
}

func (h *harness) updatePod(t *testing.T, name string, mutate func(*corev1.Pod)) {
	t.Helper()
	ctx := context.Background()
	p, err := h.cs.CoreV1().Pods(h.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mutate(p)
	if _, err := h.cs.CoreV1().Pods(h.ns).Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// apiReads counts the API server Gets of pods and StatefulSets so far.
func (h *harness) apiReads() int {
	n := 0
	for _, a := range h.cs.Actions() {
		if a.GetVerb() == "get" && (a.GetResource().Resource == "pods" || a.GetResource().Resource == "statefulsets") {
			if _, ok := a.(k8stesting.GetAction); ok {
				n++
			}
		}
	}
	return n
}

func TestSupervisorKeepsAnAgentUp(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	pod := target.Pod()
	h.track(target)

	s := h.session(t, target, nil)
	h.wantCounts(pod, map[string]int{"probe": 1, "stage": 1, "serve": 1})
	if o := h.pool.Observe(testCluster, target.StatefulSet); o.Live != backend.LiveStatusRunning || o.Agent != AgentConnected {
		t.Fatalf("observation %+v", o)
	}
	// The verification reads came from the informers.
	reads := h.apiReads()

	// The session's transport drops: the supervisor reconnects at once, on
	// the fast path.
	_ = s.stream.Close()
	waitDone(t, s.Done(), "the dropped session")
	s2 := h.session(t, target, s)
	h.wantCounts(pod, map[string]int{"probe": 1, "stage": 1, "serve": 2})
	if out := run(t, s2.Client(), "echo back"); out != "back\n" {
		t.Fatalf("ran %q", out)
	}
	if got := h.apiReads(); got != reads {
		t.Fatalf("the reconnect read the API server %d times; want the informers' copies", got-reads)
	}

	// Steady state: no exec, whatever Track and the informers repeat.
	h.track(target)
	h.updatePod(t, pod, func(p *corev1.Pod) { p.Annotations = map[string]string{"x": "y"} })
	time.Sleep(300 * time.Millisecond)
	h.wantCounts(pod, map[string]int{"probe": 1, "stage": 1, "serve": 2})
	if h.pool.Session(target) != s2 {
		t.Fatal("the session changed in steady state")
	}
}

// An agent that keeps dying as soon as it connects is a failing pod: the
// first quick drop reconnects at once, the next ones wait the backoff
// instead of looping on exec upgrades.
func TestSupervisorBacksOffSessionsThatKeepDropping(t *testing.T) {
	const backoff = 400 * time.Millisecond
	h := newHarness(t, PoolOptions{BackoffBase: backoff, BackoffCap: backoff, Rand: func() float64 { return 1 }})
	target := h.addPod("one")
	h.track(target)
	s := h.session(t, target, nil)
	var waits []time.Duration
	for range 3 {
		dropped := time.Now()
		_ = s.stream.Close()
		waitDone(t, s.Done(), "the dropped session")
		s = h.session(t, target, s)
		waits = append(waits, time.Since(dropped))
	}
	// The first drop is an ordinary one; the two after it are failures.
	for i, w := range waits[1:] {
		if w < backoff*3/4 {
			t.Fatalf("quick drop %d reconnected after %s, within the %s backoff (waits %v)", i+2, w, backoff, waits)
		}
	}
	h.wantCounts(target.Pod(), map[string]int{"probe": 1, "stage": 1, "serve": 4})
}

func TestSupervisorWaitsForThePodToRun(t *testing.T) {
	// A backoff long enough that a not-ready pod counted as a failure would
	// stall the test.
	h := newHarness(t, PoolOptions{BackoffBase: time.Hour, BackoffCap: time.Hour, Rand: func() float64 { return 1 }})
	target := h.addPod("one")
	pod := target.Pod()
	h.updatePod(t, pod, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	})
	// A job asks while the pod starts: not ready, and not a failure.
	if _, err := h.pool.EnsureAgent(context.Background(), target); !errors.Is(err, ErrPodNotReady) {
		t.Fatalf("ensure on a starting pod: %v", err)
	}
	h.track(target)
	eventually(t, "the starting pod's observation", func() bool {
		o := h.pool.Observe(testCluster, target.StatefulSet)
		return o.Live == backend.LiveStatusUnknown && o.Reason == "ContainerCreating"
	})
	time.Sleep(200 * time.Millisecond)
	h.wantCounts(pod, map[string]int{})

	h.updatePod(t, pod, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodRunning
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	})
	h.session(t, target, nil)
	h.wantCounts(pod, map[string]int{"probe": 1, "stage": 1, "serve": 1})
}

func TestSupervisorClosesTheSessionOfAPodThatStopsVerifying(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	pod := target.Pod()
	h.track(target)
	s := h.session(t, target, nil)

	// Someone adds a debug container to the running pod.
	h.updatePod(t, pod, func(p *corev1.Pod) {
		p.Spec.EphemeralContainers = append(p.Spec.EphemeralContainers, corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debugger", Image: "busybox"}})
	})
	waitDone(t, s.Done(), "the session of the pod that no longer verifies")
	eventually(t, "the tampered observation", func() bool {
		o := h.pool.Observe(testCluster, target.StatefulSet)
		return o.Live == backend.LiveStatusUnknown && o.Reason == CodeTampered && o.Tampered != nil
	})
	// It is not tried again: no exec, not on Track, not on a status change.
	h.track(target)
	h.updatePod(t, pod, func(p *corev1.Pod) { p.Annotations = map[string]string{"x": "y"} })
	time.Sleep(300 * time.Millisecond)
	h.wantCounts(pod, map[string]int{"probe": 1, "stage": 1, "serve": 1})
	if h.pool.Session(target) != nil {
		t.Fatal("a tampered pod has a session")
	}

	// fleetd's own record of the pod changes (here: the debug container is
	// what it rendered): the pod verifies again and gets a session.
	cur, _ := h.cs.CoreV1().Pods(h.ns).Get(context.Background(), pod, metav1.GetOptions{})
	target.Expect.SpecHash = testHash(&cur.Spec)
	h.track(target)
	h.session(t, target, nil)
	if o := h.pool.Observe(testCluster, target.StatefulSet); o.Tampered != nil || o.Live != backend.LiveStatusRunning {
		t.Fatalf("observation after the new record %+v", o)
	}
}

func TestSupervisorReattachesHeldCommands(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	h.track(target)
	s := h.session(t, target, nil)
	ctx := context.Background()

	e, err := s.Exec(ctx, podwire.ExecHeader{Request: execas.Request{Class: execas.ClassScript, Argv: []string{"sh", "-c", "read line; echo got $line"}}, Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	// The connection to the agent drops (an API server rollout): the
	// command is held in the pod, the supervisor reconnects and re-attaches
	// it, and its input and output carry on.
	_ = s.stream.Close()
	waitDone(t, s.Done(), "the dropped session")
	h.session(t, target, s)
	if _, err := io.WriteString(e.Stdin(), "hello\n"); err != nil {
		t.Fatal(err)
	}
	out := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(e.Stdout())
		out <- b
	}()
	waitDone(t, e.Done(), "the re-attached command")
	code, err := e.Wait()
	if got := <-out; err != nil || code != 0 || string(got) != "got hello\n" {
		t.Fatalf("the re-attached command: %q exit %d %v", got, code, err)
	}
}

func TestSupervisorFailsTheHeldCommandsOfAReplacedPod(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	h.track(target)
	s := h.session(t, target, nil)
	e, err := s.Exec(context.Background(), podwire.ExecHeader{Request: execas.Request{Class: execas.ClassScript, Argv: []string{"sleep", "30"}}, Resumable: true})
	if err != nil {
		t.Fatal(err)
	}
	// The pod is replaced (its processes went with it).
	h.updatePod(t, target.Pod(), func(p *corev1.Pod) { p.UID = types.UID("uid-one-replaced") })
	waitDone(t, e.Done(), "the held command of the replaced pod")
	if _, err := e.Wait(); !errors.Is(err, podwire.ErrHeldStreamGone) {
		t.Fatalf("the replaced pod's command: %v, want %v", err, podwire.ErrHeldStreamGone)
	}
}

func TestPoolObservesFromItsInformers(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	if o := h.pool.Observe(testCluster, target.StatefulSet); o.Live != backend.LiveStatusUnknown || o.Reason != errNotWatched.Error() {
		t.Fatalf("before any Track: %+v", o)
	}
	// Watched only: no agent.
	h.pool.Track([]Tracked{{Cluster: testCluster, StatefulSet: target.StatefulSet}, {Cluster: testCluster, StatefulSet: "fi-fleet-gone-k8sk8sk8"}})
	eventually(t, "the running observation", func() bool {
		o := h.pool.Observe(testCluster, target.StatefulSet)
		return o.Live == backend.LiveStatusRunning && o.Agent == AgentNone
	})
	if o := h.pool.Observe(testCluster, "fi-fleet-gone-k8sk8sk8"); o.Live != backend.LiveStatusMissing {
		t.Fatalf("an instance the listed cluster lacks: %+v", o)
	}
	h.wantCounts(target.Pod(), map[string]int{})

	// A pod that is not one of this daemon's instances is not seen.
	stray := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fi-fleet-stray-k8sk8sk8-0", Namespace: h.ns}}
	_, _ = h.cs.CoreV1().Pods(h.ns).Create(context.Background(), stray, metav1.CreateOptions{})
	if o := h.pool.Observe(testCluster, "fi-fleet-stray-k8sk8sk8"); o.Live != backend.LiveStatusMissing {
		t.Fatalf("an unlabelled pod: %+v", o)
	}

	// The StatefulSet is deleted out of band: missing.
	if err := h.cs.AppsV1().StatefulSets(h.ns).Delete(context.Background(), target.StatefulSet, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the missing observation", func() bool {
		return h.pool.Observe(testCluster, target.StatefulSet).Live == backend.LiveStatusMissing
	})
}

// A connection whose list is refused before it ever synced says why, not
// merely that it has not listed yet.
func TestPoolObservesARefusedList(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	prependReactor(h.cs, "list", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("RBAC: access denied"))
	})
	h.pool.Track([]Tracked{{Cluster: testCluster, StatefulSet: target.StatefulSet}})
	eventually(t, "the refused observation", func() bool {
		o := h.pool.Observe(testCluster, target.StatefulSet)
		return o.Live == backend.LiveStatusUnknown && o.Reason == "the connection may not watch its instances"
	})
}

func TestPoolReportsRestartsFromItsInformers(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	changes := make(chan string, 100)
	h.pool.OnChange(func(_, sts string) { changes <- sts })
	h.pool.Track([]Tracked{{Cluster: testCluster, StatefulSet: target.StatefulSet}})
	eventually(t, "the first observation", func() bool {
		return h.pool.Observe(testCluster, target.StatefulSet).Live == backend.LiveStatusRunning
	})
	h.updatePod(t, target.Pod(), func(p *corev1.Pod) {
		p.Status.ContainerStatuses[0].RestartCount = 1
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}
	})
	eventually(t, "the restart", func() bool {
		r, ok := h.pool.LastRestart(testCluster, target.StatefulSet)
		return ok && r.Cause == backend.RestartOOMKilled && r.ID == "uid-one/1"
	})
	select {
	case sts := <-changes:
		if sts != target.StatefulSet {
			t.Fatalf("change told for %s", sts)
		}
	default:
		t.Fatal("no change told")
	}
}

func TestPoolParksAnInstance(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	ends := make(chan RunEnd, 4)
	h.pool.OnRunEnded(func(_ context.Context, e RunEnd) { ends <- e })
	h.pool.Track([]Tracked{{Cluster: testCluster, StatefulSet: target.StatefulSet}})
	eventually(t, "the first observation", func() bool {
		return h.pool.Observe(testCluster, target.StatefulSet).Live == backend.LiveStatusRunning
	})
	if err := h.pool.Park(context.Background(), testCluster, target.StatefulSet); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-ends:
		if e.Why != RunParked || e.PodUID != "uid-one" || e.StatefulSet != target.StatefulSet {
			t.Fatalf("run end %+v", e)
		}
	default:
		t.Fatal("Park did not tell the run-end hook first")
	}
	set, err := h.cs.AppsV1().StatefulSets(h.ns).Get(context.Background(), target.StatefulSet, metav1.GetOptions{})
	if err != nil || set.Spec.Replicas == nil || *set.Spec.Replicas != 0 {
		t.Fatalf("after Park: %+v %v", set.Spec.Replicas, err)
	}
	eventually(t, "the stopped observation", func() bool {
		return h.pool.Observe(testCluster, target.StatefulSet).Live == backend.LiveStatusStopped
	})
}

func TestEnsureAgentRefreshesAMissingBuildAtARestage(t *testing.T) {
	h := newHarness(t, PoolOptions{})
	target := h.addPod("one")
	ctx := context.Background()
	if _, err := h.pool.EnsureAgent(ctx, target); err != nil {
		t.Fatal(err)
	}
	// The daemon is upgraded to a build the store has no pod build of yet.
	upgraded := h.upgrade(t, nil)
	h.envMu.Lock()
	delete(h.env, binstore.EnvBinaryPrefix+strings.ToUpper(runtime.GOARCH))
	h.envMu.Unlock()
	s, err := upgraded.EnsureAgent(ctx, target)
	if err != nil || s.Stale() || s.Hello().Version != "vtest-1" {
		t.Fatalf("with no build to judge against: %v stale %v", err, s != nil && s.Stale())
	}
	// The build arrives; the next lazy moment judges the session against it.
	h.envMu.Lock()
	h.env[binstore.EnvBinaryPrefix+strings.ToUpper(runtime.GOARCH)] = fakePod(t, "vtest-2")
	h.envMu.Unlock()
	restage := target
	restage.Restage = true
	s2, err := upgraded.EnsureAgent(ctx, restage)
	if err != nil || s2 == s || s2.Hello().Version != "vtest-2" {
		t.Fatalf("restage once the build is there: %v", err)
	}
}

// A supervisor retry waits the pod's own backoff after a failed bootstrap,
// and the backoff cap after a failure before any (a connection that does not
// load), never spinning.
func TestSupervisorRetryDelay(t *testing.T) {
	h := newHarness(t, PoolOptions{BackoffBase: time.Second, BackoffCap: time.Minute, Rand: func() float64 { return 0.5 }})
	target := h.addPod("one")
	if d := h.pool.retryDelay(target); d != time.Minute {
		t.Fatalf("delay before any bootstrap: %v, want the cap", d)
	}
	h.failMu.Lock()
	h.fail[target.Pod()] = errors.New("exec refused")
	h.failMu.Unlock()
	if _, err := h.pool.EnsureAgent(context.Background(), target); err == nil {
		t.Fatal("a failing pod bootstrapped")
	}
	if d := h.pool.retryDelay(target); d != 500*time.Millisecond {
		t.Fatalf("delay after a failed bootstrap: %v, want its backoff", d)
	}
}
