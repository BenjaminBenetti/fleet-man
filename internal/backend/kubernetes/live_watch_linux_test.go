//go:build !nok8s && linux

package kubernetes

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// TestLiveSupervisor runs the pool's informers and supervisor against a real
// cluster (FLEET_KUBE_TEST_CONTEXT, in fleet-it-p2-10b-* namespaces deleted
// when done): an agent is kept up by Track alone, steady state costs no exec,
// a deleted and an evicted pod are told apart and reconnected, and an
// instance that keeps running out of memory is seen as a crash loop and
// parked.
func TestLiveSupervisor(t *testing.T) {
	kctx := os.Getenv("FLEET_KUBE_TEST_CONTEXT")
	if kctx == "" {
		t.Skip("set FLEET_KUBE_TEST_CONTEXT=<context> to run the supervisor against a cluster")
	}
	image := os.Getenv("FLEET_KUBE_TEST_IMAGE")
	if image == "" {
		image = "mcr.microsoft.com/devcontainers/base:debian-12"
	}
	ctx := context.Background()
	lv := newLiveIn(t, kctx, "fleet-it-p2-10b-pool-informers-")
	v1 := lv.buildPod("vlive-1")
	sts, secret, renderHash := lv.applyInstance(image)
	pod := lv.waitRunning(sts)
	target := PodTarget{
		Cluster: lv.rec.ID, Fleet: "live", Instance: "one", StatefulSet: sts.Name, SpecSecret: secret,
		Expect: Expect{ImageDigests: []string{pod.Status.ContainerStatuses[0].ImageID}, SpecHash: renderHash, StatefulSetUID: sts.UID},
	}
	pool, execs := lv.pool(v1)
	var endsMu sync.Mutex
	var ends []RunEnd
	pool.OnRunEnded(func(_ context.Context, e RunEnd) {
		endsMu.Lock()
		ends = append(ends, e)
		endsMu.Unlock()
	})
	runEnds := func(why RunEndReason) int {
		endsMu.Lock()
		defer endsMu.Unlock()
		n := 0
		for _, e := range ends {
			if e.Why == why {
				n++
			}
		}
		return n
	}
	tracked := []Tracked{{Cluster: lv.rec.ID, StatefulSet: sts.Name, Target: &target}}
	pool.Track(tracked)

	waitFor := func(what string, d time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(d)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	sessionOn := func(uid types.UID) *AgentSession {
		t.Helper()
		var s *AgentSession
		waitFor("a session on pod "+string(uid), 3*time.Minute, func() bool {
			s = pool.Session(target)
			return s != nil && s.Pod().UID == uid
		})
		return s
	}
	podUID := func() types.UID {
		t.Helper()
		var uid types.UID
		waitFor("the new pod running", 3*time.Minute, func() bool {
			p, err := lv.cs.CoreV1().Pods(lv.ns).Get(ctx, target.Pod(), metav1.GetOptions{})
			if err != nil || !podRunning(p) {
				return false
			}
			uid = p.UID
			return true
		})
		return uid
	}

	s := sessionOn(pod.UID)
	if got := execs.snapshot(); got["probe"] != 1 || got["stage"] != 1 || got["serve"] != 1 {
		t.Fatalf("the supervisor's bootstrap ran %v", got)
	}
	if o := pool.Observe(lv.rec.ID, sts.Name); o.Live != backend.LiveStatusRunning || o.Agent != AgentConnected || o.Node == "" {
		t.Fatalf("observation %+v", o)
	}
	if out := run(t, s.Client(), "echo up"); out != "up\n" {
		t.Fatalf("ran %q", out)
	}

	// Steady state: no exec upgrade at all.
	before := execs.snapshot()
	time.Sleep(20 * time.Second)
	if got := execs.snapshot(); !sameCounts(got, before) {
		t.Fatalf("steady state ran execs: %v, then %v", before, got)
	}

	t.Run("deleted pod", func(t *testing.T) {
		if err := lv.cs.CoreV1().Pods(lv.ns).Delete(ctx, target.Pod(), metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		uid := podUID()
		waitFor("the deleted pod's restart", time.Minute, func() bool {
			r, ok := pool.LastRestart(lv.rec.ID, sts.Name)
			return ok && r.Cause == backend.RestartPodDeleted && r.ID == string(uid)+"/0"
		})
		sessionOn(uid)
		if runEnds(RunTerminating) == 0 {
			t.Fatal("the terminating pod's run was not told")
		}
	})

	t.Run("evicted pod", func(t *testing.T) {
		old := pool.Session(target).Pod().UID
		if err := lv.cs.PolicyV1().Evictions(lv.ns).Evict(ctx, &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: target.Pod(), Namespace: lv.ns}}); err != nil {
			t.Fatal(err)
		}
		var uid types.UID
		waitFor("the replacement pod", 3*time.Minute, func() bool {
			uid = podUID()
			return uid != old
		})
		waitFor("the evicted pod's restart", time.Minute, func() bool {
			r, ok := pool.LastRestart(lv.rec.ID, sts.Name)
			return ok && r.Cause == backend.RestartNodeDrained && r.ID == string(uid)+"/0"
		})
		sessionOn(uid)
	})

	t.Run("out-of-memory loop parked", func(t *testing.T) {
		oom, _, _ := lv.applyInstanceNamed("fi-live-oom-k8sl1ve1", image, func(spec *corev1.PodSpec) {
			c := &spec.Containers[0]
			c.Command = []string{"sh", "-c", "sleep 2; tail /dev/zero"}
			c.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("48Mi")}
			c.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("48Mi")}
		})
		pool.Track(append(tracked, Tracked{Cluster: lv.rec.ID, StatefulSet: oom.Name}))
		waitFor("an out-of-memory restart", 3*time.Minute, func() bool {
			r, ok := pool.LastRestart(lv.rec.ID, oom.Name)
			return ok && r.Cause == backend.RestartOOMKilled
		})
		if o := pool.Observe(lv.rec.ID, oom.Name); !o.SessionsEnded {
			t.Fatalf("an OOM restart did not end the sessions: %+v", o)
		}
		var loop *CrashLoop
		waitFor("the crash loop", 5*time.Minute, func() bool {
			loop = pool.Observe(lv.rec.ID, oom.Name).CrashLoop
			return loop != nil
		})
		if !loop.OOM || loop.MemoryLimit != "48Mi" || loop.Code() != CodeOOMKilled {
			t.Fatalf("loop %+v", loop)
		}
		t.Logf("parking: %s", loop.Message())
		if err := pool.Park(ctx, lv.rec.ID, oom.Name); err != nil {
			t.Fatal(err)
		}
		if runEnds(RunParked) != 1 {
			t.Fatal("the parked instance's run was not told first")
		}
		waitFor("the parked instance stopped", time.Minute, func() bool {
			o := pool.Observe(lv.rec.ID, oom.Name)
			return o.Live == backend.LiveStatusStopped && o.CrashLoop == nil
		})
		// The supervised instance went on undisturbed.
		if o := pool.Observe(lv.rec.ID, sts.Name); o.Live != backend.LiveStatusRunning || o.Agent != AgentConnected {
			t.Fatalf("the other instance: %+v", o)
		}
	})
}

func sameCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
