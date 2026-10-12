//go:build !nok8s

package server

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
)

// deepRig stubs the full preflight: a deep one reports the two-pod probe
// running, waits for release, and finds that two pods on one node cannot
// mount one volume (told: the copy tier) or, unless told, cannot tell; a
// plain one is the real preflight of the fake cluster. deeps counts the deep
// runs.
type deepRig struct {
	mu      sync.Mutex
	deeps   int
	release chan struct{}
	told    bool
}

func stubDeepPreflight(t *testing.T, told bool) *deepRig {
	t.Helper()
	r := &deepRig{release: make(chan struct{}), told: told}
	old := fullPreflight
	fullPreflight = func(ctx context.Context, tgt preflight.Target, report func(*fleetgrpc.KubeCheck)) preflight.Result {
		if !tgt.Deep {
			return old(ctx, tgt, report)
		}
		r.mu.Lock()
		r.deeps++
		r.mu.Unlock()
		tgt.Deep = false
		res := old(ctx, tgt, report)
		report(&fleetgrpc.KubeCheck{Name: preflight.CheckSharePods, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_RUNNING})
		select {
		case <-r.release:
		case <-ctx.Done():
			return res
		}
		if r.told {
			no := false
			res.MultiPodRWO, res.DeepCheckedAt = &no, time.Now()
			res.Capabilities.ShareMode = string(topology.Copy)
			report(&fleetgrpc.KubeCheck{Name: preflight.CheckSharePods, Result: fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_FAIL, Detail: "the second pod could not mount it"})
		}
		return res
	}
	t.Cleanup(func() { fullPreflight = old })
	return r
}

func (r *deepRig) runs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deeps
}

// checkLog collects the checks a create was shown.
type checkLog struct {
	mu     sync.Mutex
	checks []string
}

func (l *checkLog) add(c *fleetgrpc.KubeCheck) {
	l.mu.Lock()
	l.checks = append(l.checks, c.GetName()+"="+c.GetResult().String())
	l.mu.Unlock()
}

func (l *checkLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.checks...)
}

// The first fleet share on a connection no deep test has told waits for one:
// two creates at once wait for the same run, each shown its own checks (the
// deep test's, not the whole preflight's) from where it joined, and both get
// the share mode it found; a later create gets the facts at once.
func TestBackendDeepFactsRunTheDeepTestOnce(t *testing.T) {
	f := newKubeFixture(t)
	f.start()
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", DisplayName: "dev", Namespace: "fleet"}).GetCluster().GetId()
	m := f.svc.kube
	if _, err := m.backendFacts(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	r := stubDeepPreflight(t, true)

	type got struct {
		mode string
		err  error
	}
	logs := []*checkLog{{}, {}}
	results := make(chan got, 2)
	for _, l := range logs {
		go func() {
			facts, err := m.backendDeepFacts(context.Background(), id, l.add)
			results <- got{facts.Capabilities.GetShareMode(), err}
		}()
	}
	// Both wait on the one run.
	deadline := time.Now().Add(10 * time.Second)
	for len(logs[0].all()) == 0 || len(logs[1].all()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the creates were shown %v and %v; want the running probe", logs[0].all(), logs[1].all())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := r.runs(); n != 1 {
		t.Fatalf("%d deep tests for two creates", n)
	}
	close(r.release)
	for range 2 {
		g := <-results
		if g.err != nil || g.mode != string(topology.Copy) {
			t.Fatalf("facts after the deep test: mode %q, %v; want the copy tier it found", g.mode, g.err)
		}
	}
	for i, l := range logs {
		checks := l.all()
		running, failed := slices.Index(checks, "share_pods=KUBE_CHECK_RESULT_RUNNING"), slices.Index(checks, "share_pods=KUBE_CHECK_RESULT_FAIL")
		if running < 0 || failed < running {
			t.Errorf("create %d was shown %v; want the probe running, then failed", i, checks)
		}
		for _, c := range checks {
			if name, _, _ := strings.Cut(c, "="); !deepStepChecks[name] {
				t.Errorf("create %d was shown a check of the preflight it had: %s", i, c)
			}
		}
	}

	// Told: a later create takes the facts at once.
	start := time.Now()
	facts, err := m.backendDeepFacts(context.Background(), id, func(c *fleetgrpc.KubeCheck) { t.Errorf("shown %s with the test told", c.GetName()) })
	if err != nil || facts.Capabilities.GetShareMode() != string(topology.Copy) || r.runs() != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("after the test: mode %q, %v, %d runs", facts.Capabilities.GetShareMode(), err, r.runs())
	}
}

// A create that gives up waiting fails; the run goes on and its word is the
// connection's. One that could not tell is not run again by a create.
func TestBackendDeepFactsCancelledAndUntold(t *testing.T) {
	f := newKubeFixture(t)
	f.start()
	id := f.add(&fleetgrpc.KubeCluster{Context: "minikube", DisplayName: "dev", Namespace: "fleet"}).GetCluster().GetId()
	m := f.svc.kube
	if _, err := m.backendFacts(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	r := stubDeepPreflight(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := m.backendDeepFacts(ctx, id, nil); err == nil {
		t.Fatal("a create that gave up waiting got facts")
	}
	close(r.release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		w := m.worker(id)
		w.mu.Lock()
		running := w.firstDeep != nil
		w.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the deep test never ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := m.backendDeepFacts(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	if n := r.runs(); n != 1 {
		t.Fatalf("%d deep tests; one that could not tell is not run again by a create", n)
	}
}
