//go:build !nok8s

package kubernetes

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// tracker drives the pool's tracking with events as the informers deliver
// them, on a clock the test moves.
type tracker struct {
	t    *testing.T
	pool *Pool
	w    *watcher
	now  time.Time

	mu   sync.Mutex
	ends []RunEnd
	seen []string
}

const trackSTS = "fi-fleet-one-abcdefgh"

func newTracker(t *testing.T) *tracker {
	tr := &tracker{t: t, now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	tr.pool = NewPool(PoolOptions{Now: func() time.Time {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		return tr.now
	}})
	t.Cleanup(tr.pool.Close)
	tr.pool.OnRunEnded(func(_ context.Context, e RunEnd) {
		tr.mu.Lock()
		tr.ends = append(tr.ends, e)
		tr.mu.Unlock()
	})
	tr.pool.OnChange(func(cluster, sts string) {
		tr.mu.Lock()
		tr.seen = append(tr.seen, cluster+"/"+sts)
		tr.mu.Unlock()
	})
	tr.w = &watcher{pool: tr.pool, cluster: &Cluster{Record: fleet.KubeCluster{ID: "c1"}}, ns: "ns"}
	return tr
}

func (tr *tracker) advance(d time.Duration) {
	tr.mu.Lock()
	tr.now = tr.now.Add(d)
	tr.mu.Unlock()
}

func (tr *tracker) set(n int32) {
	tr.pool.setEvent(tr.w, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: trackSTS, Namespace: "ns"}, Spec: appsv1.StatefulSetSpec{Replicas: &n}}, false)
}

func (tr *tracker) pod(p *corev1.Pod) { tr.pool.podEvent(tr.w, trackSTS, p, false) }

func (tr *tracker) deleted(p *corev1.Pod) { tr.pool.podEvent(tr.w, trackSTS, p, true) }

func (tr *tracker) restart() (backend.Restart, bool) { return tr.pool.LastRestart("c1", trackSTS) }

func (tr *tracker) wantNoRestart() {
	tr.t.Helper()
	if r, ok := tr.restart(); ok {
		tr.t.Fatalf("restart %+v, want none", r)
	}
}

func (tr *tracker) wantRestart(id string, cause backend.RestartCause) backend.Restart {
	tr.t.Helper()
	r, ok := tr.restart()
	if !ok || r.ID != id || r.Cause != cause {
		tr.t.Fatalf("restart %+v (%v), want %s %q", r, ok, id, cause)
	}
	return r
}

func (tr *tracker) runEnds() []RunEnd {
	// The hook runs on goroutines of its own.
	time.Sleep(20 * time.Millisecond)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]RunEnd(nil), tr.ends...)
}

// trackedPod is an instance pod with uid whose dev container restarted
// count times, running unless mutated.
func trackedPod(uid string, count int32, mutate ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: trackSTS + "-0", Namespace: "ns", UID: types.UID(uid)},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: DevContainer, Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: DevContainer, RestartCount: count,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Date(2026, 10, 10, 12, 0, int(count), 0, time.UTC))}},
		}}},
	}
	for _, m := range mutate {
		m(p)
	}
	return p
}

func lastExit(reason string, code int32) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: reason, ExitCode: code}
	}
}

func disrupted(reason string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: reason})
	}
}

func terminating(p *corev1.Pod) {
	now := metav1.Now()
	p.DeletionTimestamp = &now
}

// going is p as the informer delivers it once its deletion started (a new
// object, as an informer's always is).
func going(p *corev1.Pod) *corev1.Pod {
	p = p.DeepCopy()
	terminating(p)
	return p
}

func TestTrackBaselineIsNoRestart(t *testing.T) {
	tr := newTracker(t)
	tr.set(1)
	// fleetd starts watching a pod that restarted twice before: nothing.
	tr.pod(trackedPod("u1", 2, lastExit("OOMKilled", 137)))
	tr.wantNoRestart()
	tr.pod(trackedPod("u1", 2, lastExit("OOMKilled", 137)))
	tr.wantNoRestart()
	if o := tr.pool.Observe("c1", trackSTS); o.CrashLoop != nil || o.Restart != nil {
		t.Fatalf("observation %+v", o)
	}
}

func TestTrackContainerRestarts(t *testing.T) {
	tr := newTracker(t)
	tr.set(1)
	tr.pod(trackedPod("u1", 0))
	tr.pod(trackedPod("u1", 1, lastExit("OOMKilled", 137)))
	r := tr.wantRestart("u1/1", backend.RestartOOMKilled)
	if !r.At.Equal(time.Date(2026, 10, 10, 12, 0, 1, 0, time.UTC)) {
		t.Fatalf("restart at %v, want the container's start", r.At)
	}
	if o := tr.pool.Observe("c1", trackSTS); !o.SessionsEnded {
		t.Fatal("an OOM restart did not end the sessions")
	}
	tr.advance(10 * time.Minute)
	tr.pod(trackedPod("u1", 2, lastExit("Error", 1)))
	tr.wantRestart("u1/2", backend.RestartCrashed)
	if o := tr.pool.Observe("c1", trackSTS); o.SessionsEnded || o.CrashLoop != nil {
		t.Fatalf("observation after a crash %+v", o)
	}
	ends := tr.runEnds()
	sort.Slice(ends, func(i, j int) bool { return ends[i].RestartCount < ends[j].RestartCount })
	if len(ends) != 2 || ends[0].RestartCount != 0 || ends[1].RestartCount != 1 || ends[0].Why != RunRestarted || ends[0].PodUID != "u1" || ends[0].Pod != trackSTS+"-0" {
		t.Fatalf("run ends %+v", ends)
	}
}

func TestTrackPodReplacements(t *testing.T) {
	cases := []struct {
		name string
		gone func(*tracker, *corev1.Pod)
		want backend.RestartCause
	}{
		{"drained", func(tr *tracker, p *corev1.Pod) {
			disrupted("EvictionByEvictionAPI")(p)
			terminating(p)
			tr.pod(p)
			tr.deleted(p)
		}, backend.RestartNodeDrained},
		{"node lost", func(tr *tracker, p *corev1.Pod) {
			disrupted("DeletionByTaintManager")(p)
			tr.deleted(p)
		}, backend.RestartNodeLost},
		{"node deleted", func(tr *tracker, p *corev1.Pod) {
			disrupted("DeletionByPodGC")(p)
			tr.deleted(p)
		}, backend.RestartNodeLost},
		{"preempted", func(tr *tracker, p *corev1.Pod) {
			disrupted("PreemptionByScheduler")(p)
			tr.deleted(p)
		}, backend.RestartEvicted},
		{"kubelet eviction", func(tr *tracker, p *corev1.Pod) {
			p.Status.Phase, p.Status.Reason = corev1.PodFailed, "Evicted"
			tr.pod(p)
			tr.deleted(p)
		}, backend.RestartEvicted},
		{"deleted", func(tr *tracker, p *corev1.Pod) {
			terminating(p)
			tr.pod(p)
			tr.deleted(p)
		}, backend.RestartPodDeleted},
		{"deletion missed: the old pod's last state names it", func(tr *tracker, p *corev1.Pod) {
			disrupted("EvictionByEvictionAPI")(p)
			tr.pod(p)
		}, backend.RestartNodeDrained},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTracker(t)
			tr.set(1)
			tr.pod(trackedPod("u1", 0))
			tc.gone(tr, trackedPod("u1", 0))
			tr.wantNoRestart()
			tr.pod(trackedPod("u2", 0))
			tr.wantRestart("u2/0", tc.want)
		})
	}
}

func TestTrackStopAndStartAreNoRestart(t *testing.T) {
	for _, podFirst := range []bool{false, true} {
		tr := newTracker(t)
		tr.set(1)
		old := trackedPod("u1", 0)
		tr.pod(old)
		// A Fleet stop: the StatefulSet goes to 0 and its pod goes, in
		// either order as two watches deliver them.
		if podFirst {
			tr.pod(going(old))
			tr.deleted(going(old))
			tr.set(0)
		} else {
			tr.set(0)
			tr.pod(going(old))
			tr.deleted(going(old))
		}
		if o := tr.pool.Observe("c1", trackSTS); o.Live != backend.LiveStatusUnknown {
			// Not watched: the connection has no running watcher here.
			t.Fatalf("observation %+v", o)
		}
		tr.set(1)
		tr.pod(trackedPod("u2", 0))
		tr.wantNoRestart()
		// The terminating pod's run is still one to save.
		if ends := tr.runEnds(); len(ends) != 1 || ends[0].Why != RunTerminating {
			t.Fatalf("run ends %+v", ends)
		}
	}
}

// A stop and start whose pod events the watch missed (a relist shows the
// new pod as an update of the old one) is still no restart: the scale to 0
// was seen.
func TestTrackStopAndStartWithTheDeletionMissed(t *testing.T) {
	tr := newTracker(t)
	tr.set(1)
	tr.pod(trackedPod("u1", 0))
	tr.set(0)
	tr.set(1)
	tr.pod(trackedPod("u2", 0))
	tr.wantNoRestart()
}

func TestTrackExpectedReplacement(t *testing.T) {
	tr := newTracker(t)
	tr.set(1)
	tr.pod(trackedPod("u1", 0))
	tr.pool.ExpectReplacement("c1", trackSTS, "")
	tr.deleted(trackedPod("u1", 0))
	tr.pod(trackedPod("u2", 0))
	tr.wantNoRestart()

	tr.pool.ExpectReplacement("c1", trackSTS, backend.RestartFleetMoved)
	tr.deleted(trackedPod("u2", 0))
	tr.pod(trackedPod("u3", 0))
	tr.wantRestart("u3/0", backend.RestartFleetMoved)

	// An announcement that waited too long is let go.
	tr.pool.ExpectReplacement("c1", trackSTS, "")
	tr.advance(expectationTTL + time.Second)
	tr.deleted(trackedPod("u3", 0))
	tr.pod(trackedPod("u4", 0))
	tr.wantRestart("u4/0", backend.RestartPodDeleted)
}

// An evacuation stops the instance and starts it again on another node:
// though a stop and a start, the cluster ended its processes, so the new pod
// is a restart, told as the fleet's move, even when the drain evicted the
// pod first.
func TestTrackAMoveIsARestart(t *testing.T) {
	tr := newTracker(t)
	tr.set(1)
	tr.pod(trackedPod("u1", 0))
	tr.set(0)
	tr.deleted(trackedPod("u1", 0))
	tr.pool.ExpectReplacement("c1", trackSTS, backend.RestartFleetMoved)
	tr.set(1)
	tr.pod(trackedPod("u2", 0))
	tr.wantRestart("u2/0", backend.RestartFleetMoved)

	// The drain evicted it first; the StatefulSet's replacement waited on
	// the cordoned node until the move stopped it.
	tr.pod(going(trackedPod("u2", 0, disrupted("EvictionByEvictionAPI"))))
	tr.deleted(trackedPod("u2", 0))
	tr.pod(trackedPod("u3", 0, func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }))
	tr.wantRestart("u3/0", backend.RestartNodeDrained)
	tr.set(0)
	tr.deleted(trackedPod("u3", 0))
	tr.pool.ExpectReplacement("c1", trackSTS, backend.RestartFleetMoved)
	tr.set(1)
	tr.pod(trackedPod("u4", 0))
	tr.wantRestart("u4/0", backend.RestartFleetMoved)
	if l := tr.pool.Observe("c1", trackSTS).CrashLoop; l != nil {
		t.Fatalf("a move counted toward a crash loop: %+v", l)
	}
}

func TestTrackCrashLoop(t *testing.T) {
	t.Run("three restarts in five minutes", func(t *testing.T) {
		tr := newTracker(t)
		tr.set(1)
		tr.pod(trackedPod("u1", 0))
		for n := int32(1); n <= 2; n++ {
			tr.advance(time.Minute)
			tr.pod(trackedPod("u1", n, lastExit("OOMKilled", 137)))
			if o := tr.pool.Observe("c1", trackSTS); o.CrashLoop != nil {
				t.Fatalf("a loop after %d restarts", n)
			}
		}
		tr.advance(time.Minute)
		tr.pod(trackedPod("u1", 3, lastExit("OOMKilled", 137)))
		l := tr.pool.Observe("c1", trackSTS).CrashLoop
		if l == nil || !l.OOM || l.Restarts != 3 || l.MemoryLimit != "1Gi" || l.Code() != CodeOOMKilled {
			t.Fatalf("loop %+v", l)
		}
		if msg := l.Message(); !strings.HasPrefix(msg, CodeOOMKilled+": ") || !strings.Contains(msg, "memory limit 1Gi") || !strings.Contains(msg, "raise memory_limit") {
			t.Fatalf("message %q", msg)
		}
		// Parked: the scale-down ends the loop.
		tr.set(0)
		if o := tr.pool.Observe("c1", trackSTS); o.CrashLoop != nil {
			t.Fatalf("a loop after the scale-down: %+v", o.CrashLoop)
		}
		// A start brings a fresh pod: a fresh count.
		tr.set(1)
		tr.pod(trackedPod("u2", 0))
		tr.pod(trackedPod("u2", 1, lastExit("Error", 2)))
		if o := tr.pool.Observe("c1", trackSTS); o.CrashLoop != nil {
			t.Fatalf("a loop right after a start: %+v", o.CrashLoop)
		}
	})
	t.Run("restarts spread out", func(t *testing.T) {
		tr := newTracker(t)
		tr.set(1)
		tr.pod(trackedPod("u1", 0))
		for n := int32(1); n <= 5; n++ {
			tr.advance(3 * time.Minute)
			tr.pod(trackedPod("u1", n, lastExit("Error", 1)))
		}
		if l := tr.pool.Observe("c1", trackSTS).CrashLoop; l != nil {
			t.Fatalf("loop %+v for a restart every 3 minutes", l)
		}
	})
	t.Run("several restarts in one event", func(t *testing.T) {
		tr := newTracker(t)
		tr.set(1)
		tr.pod(trackedPod("u1", 0))
		tr.pod(trackedPod("u1", 4, lastExit("Error", 3)))
		l := tr.pool.Observe("c1", trackSTS).CrashLoop
		if l == nil || l.OOM || l.ExitCode != 3 || l.Code() != CodeCrashLoop {
			t.Fatalf("loop %+v", l)
		}
		if msg := l.Message(); !strings.Contains(msg, "exit code 3") || !strings.Contains(msg, "fleet logs") {
			t.Fatalf("message %q", msg)
		}
	})
	t.Run("replaced pods are no crashes", func(t *testing.T) {
		// A rolling node upgrade drains the instance off node after node:
		// every move is a restart, none a crash.
		tr := newTracker(t)
		tr.set(1)
		tr.pod(trackedPod("u0", 0))
		for i := 1; i <= 4; i++ {
			tr.advance(30 * time.Second)
			old := trackedPod("u"+strconv.Itoa(i-1), 0, disrupted("EvictionByEvictionAPI"))
			tr.pod(going(old))
			tr.deleted(going(old))
			id := "u" + strconv.Itoa(i)
			tr.pod(trackedPod(id, 0))
			tr.wantRestart(id+"/0", backend.RestartNodeDrained)
		}
		if l := tr.pool.Observe("c1", trackSTS).CrashLoop; l != nil {
			t.Fatalf("loop %+v for four drains", l)
		}
		// Crashes still count, across a move.
		for n := int32(1); n <= 3; n++ {
			tr.advance(time.Second)
			tr.pod(trackedPod("u4", n, lastExit("Error", 1)))
		}
		if l := tr.pool.Observe("c1", trackSTS).CrashLoop; l == nil || l.Restarts != 3 {
			t.Fatalf("loop %+v after three crashes", l)
		}
	})
	t.Run("back-off", func(t *testing.T) {
		tr := newTracker(t)
		tr.set(1)
		tr.pod(trackedPod("u1", 1, func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
			lastExit("Error", 1)(p)
		}))
		l := tr.pool.Observe("c1", trackSTS).CrashLoop
		if l == nil || !l.BackOff || l.Restarts != 0 {
			t.Fatalf("loop %+v", l)
		}
		if msg := l.Message(); !strings.Contains(msg, "backing it off") {
			t.Fatalf("message %q", msg)
		}
	})
}

func TestTrackChangesAreTold(t *testing.T) {
	tr := newTracker(t)
	tr.set(1)
	tr.pod(trackedPod("u1", 0))
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.seen) != 2 || tr.seen[0] != "c1/"+trackSTS {
		t.Fatalf("changes told %v", tr.seen)
	}
}

func TestRestartHint(t *testing.T) {
	for cause, want := range map[backend.RestartCause]string{
		backend.RestartOOMKilled:   "raise memory_limit in the cluster's resources",
		backend.RestartEvicted:     "raise memory_request in the cluster's resources",
		backend.RestartNodeDrained: "turn on protect_from_drain in the cluster's settings",
		backend.RestartNodeLost:    "",
		backend.RestartCrashed:     "",
		backend.RestartPodDeleted:  "",
		backend.RestartFleetMoved:  "",
	} {
		if got := RestartHint(cause); got != want {
			t.Errorf("RestartHint(%q) = %q, want %q", cause, got, want)
		}
	}
}

// The pod informer can deliver before the StatefulSet one: a pod that goes
// while the StatefulSet is not seen yet is judged by what comes next.
func TestTrackBeforeTheStatefulSetIsSeen(t *testing.T) {
	tr := newTracker(t)
	tr.pod(trackedPod("u1", 0))
	tr.deleted(trackedPod("u1", 0))
	tr.pod(trackedPod("u2", 0))
	tr.wantRestart("u2/0", backend.RestartPodDeleted)

	tr = newTracker(t)
	tr.pod(trackedPod("u1", 0))
	tr.deleted(going(trackedPod("u1", 0)))
	tr.set(0) // it was a stop
	tr.set(1)
	tr.pod(trackedPod("u2", 0))
	tr.wantNoRestart()
}
