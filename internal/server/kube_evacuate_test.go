//go:build !nok8s

package server

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// evacStub stands in for the pool's Evacuate: it records each move and
// answers with result (outcomes per member: started unless failed), holding
// the move until released when hold is set.
type evacStub struct {
	mu      sync.Mutex
	moves   []kubernetes.Evacuation
	blocked string
	failed  map[string]kubernetes.EvacuationOutcome
	hold    chan struct{}
	ended   chan kubernetes.EvacuationResult
}

func (s *evacStub) evacuate(_ context.Context, e kubernetes.Evacuation) (kubernetes.EvacuationResult, error) {
	s.mu.Lock()
	s.moves = append(s.moves, e)
	hold, blocked := s.hold, s.blocked
	s.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if blocked != "" {
		return kubernetes.EvacuationResult{Blocked: blocked}, nil
	}
	res := kubernetes.EvacuationResult{To: "n2"}
	for _, m := range e.Members {
		o, ok := s.failed[m.Instance]
		if !ok {
			o = kubernetes.EvacuationOutcome{Stopped: true, Started: true}
		}
		o.Instance = m.Instance
		res.Outcomes = append(res.Outcomes, o)
	}
	return res, nil
}

func (s *evacStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.moves)
}

func (s *evacStub) last() kubernetes.Evacuation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.moves[len(s.moves)-1]
}

// newEvacRig is a kubePods rig whose moves go to the stub, over the fleet's
// running instances.
func newEvacRig(t *testing.T, stub *evacStub, instances ...*fleet.Instance) *kubePodsRig {
	t.Helper()
	stub.ended = make(chan kubernetes.EvacuationResult, 10)
	r := newKubePodsRigWith(t, func(k *kubePods) {
		k.evac.evacuate = stub.evacuate
		k.evac.members = func(cluster, fleetName string) (string, []evacMember) {
			st, _ := state.Load()
			var ms []evacMember
			for _, inst := range st.Fleets[fleetName].Instances {
				if inst.Status == fleet.StatusRunning && inst.KubeCluster == cluster {
					ms = append(ms, evacMember{name: inst.Name, sts: kubeSTS(inst.Name), key: inst.KubeKey})
				}
			}
			return "fs-alpha-share", ms
		}
		k.evac.done = func(_ evacKey, res kubernetes.EvacuationResult) { stub.ended <- res }
	}, instances...)
	// The connection's status is pushed on every change of a move: no
	// registry here, and nothing for the daemon's pool to learn.
	r.svc.kube.loadRegistry = func() (*state.KubeClusters, error) { return &state.KubeClusters{}, nil }
	r.svc.kube.configurePool = func([]fleet.KubeCluster) {}
	return r
}

func (r *kubePodsRig) waitMoveEnded(stub *evacStub) kubernetes.EvacuationResult {
	r.t.Helper()
	select {
	case res := <-stub.ended:
		return res
	case <-time.After(5 * time.Second):
		r.t.Fatal("the move did not end")
	}
	return kubernetes.EvacuationResult{}
}

// cordoned is a running holder's observation on node n1, which the cluster
// empties.
func cordoned() kubernetes.Observation {
	return kubernetes.Observation{Live: backend.LiveStatusRunning, Node: "n1", Holder: true, Disrupted: "cordoned"}
}

func TestKubeEvacuationMovesTheFleet(t *testing.T) {
	stub := &evacStub{hold: make(chan struct{})}
	r := newEvacRig(t, stub, kubeInstance("one", fleet.StatusRunning), kubeInstance("two", fleet.StatusRunning), kubeInstance("three", fleet.StatusStopped))
	r.waitTracked()
	r.pool.observe(kubeSTS("one"), cordoned())
	r.pool.observe(kubeSTS("two"), cordoned())
	r.eventually("the move", func() bool { return stub.count() == 1 })
	e := stub.last()
	if e.Cluster != "c1" || e.Node != "n1" || e.Why != "cordoned" || e.Fleet != "alpha" || e.Share != "fs-alpha-share" || len(e.Members) != 2 {
		t.Fatalf("move %+v", e)
	}
	// While it runs: every row of the fleet says so (the stopped one's too),
	// and so does the connection's status.
	for _, name := range []string{"one", "two", "three"} {
		r.eventually(name+" moving", func() bool { return r.runtime(name).GetKube().GetNodeCordonedMoving() })
	}
	if moving, warnings := r.svc.kubePods.evac.status("c1"); !moving || len(warnings) != 1 || warnings[0] != "node cordoned: fleet moving (alpha off n1, cordoned)" {
		t.Fatalf("status %v %q", moving, warnings)
	}
	w := &kubeWorker{m: r.svc.kube, wake: make(chan struct{}, 1), rec: fleet.KubeCluster{ID: "c1"}}
	w.haveFull, w.full = true, preflight.Result{State: fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_READY, Capabilities: &fleetgrpc.KubeCapabilities{}}
	if st := w.status(); !st.GetCapabilities().GetNodeCordonedMoving() || !strings.Contains(strings.Join(st.GetWarnings(), "\n"), "node cordoned: fleet moving") {
		t.Fatalf("connection status %v %q", st.GetCapabilities().GetNodeCordonedMoving(), st.GetWarnings())
	}
	// Asked again while it runs: still one move.
	r.pool.observe(kubeSTS("one"), cordoned())
	time.Sleep(30 * time.Millisecond)
	if n := stub.count(); n != 1 {
		t.Fatalf("%d moves for one", n)
	}

	close(stub.hold)
	if res := r.waitMoveEnded(stub); len(res.Outcomes) != 2 {
		t.Fatalf("result %+v", res)
	}
	r.eventually("moved", func() bool { return !r.runtime("one").GetKube().GetNodeCordonedMoving() })
	if moving, warnings := r.svc.kubePods.evac.status("c1"); moving || len(warnings) != 0 {
		t.Fatalf("status after %v %q", moving, warnings)
	}
	// Still seen on the node a moment later (the pods the move made are on
	// another node, but an event may lag): not asked again at once.
	r.pool.observe(kubeSTS("one"), cordoned())
	time.Sleep(30 * time.Millisecond)
	if n := stub.count(); n != 1 {
		t.Fatalf("asked again at once: %d moves", n)
	}
	// After the retry pause it is.
	r.svc.kubePods.evac.mu.Lock()
	h := r.svc.kubePods.evac.held[evacKey{"c1", "alpha"}]
	h.until = time.Now().Add(-time.Second)
	r.svc.kubePods.evac.held[evacKey{"c1", "alpha"}] = h
	r.svc.kubePods.evac.mu.Unlock()
	r.pool.observe(kubeSTS("one"), cordoned())
	r.eventually("the second move", func() bool { return stub.count() == 2 })
	r.waitMoveEnded(stub)
}

// The cluster's restart of a moved instance is told as the move ("sessions
// reset: fleet moved"); the move's own start ran its post-start work.
func TestKubeEvacuationRestartRunsNoSecondPostStart(t *testing.T) {
	r := newKubePodsRig(t, kubeInstance("one", fleet.StatusRunning))
	r.waitTracked()
	r.pool.observe(kubeSTS("one"), kubernetes.Observation{Live: backend.LiveStatusRunning, Node: "n2",
		Restart: &backend.Restart{ID: "u2/0", Cause: backend.RestartFleetMoved, At: time.Now()}})
	r.eventually("the notice", func() bool { return r.runtime("one").GetKube().GetRestartCause() == string(backend.RestartFleetMoved) })
	time.Sleep(50 * time.Millisecond)
	if got := r.restarts(); len(got) != 0 {
		t.Fatalf("post-start re-run after the move's own start: %v", got)
	}
}

func TestKubeEvacuationOfAFleetThatCannotMove(t *testing.T) {
	stub := &evacStub{blocked: "its share is node-local, class standard"}
	r := newEvacRig(t, stub, kubeInstance("one", fleet.StatusRunning))
	r.waitTracked()
	r.pool.observe(kubeSTS("one"), cordoned())
	r.waitMoveEnded(stub)
	r.eventually("the row", func() bool {
		return r.runtime("one").GetKube().GetMoveBlocked() == "its share is node-local, class standard"
	})
	if _, warnings := r.svc.kubePods.evac.status("c1"); len(warnings) != 1 || warnings[0] != "node cordoned: fleet can't move (alpha stays on n1: its share is node-local, class standard)" {
		t.Fatalf("warnings %q", warnings)
	}
	// Not asked again while the node is emptied.
	r.pool.observe(kubeSTS("one"), cordoned())
	time.Sleep(30 * time.Millisecond)
	if n := stub.count(); n != 1 {
		t.Fatalf("%d moves of a fleet that cannot move", n)
	}
	// Uncordoned: the row clears; cordoned again, it is asked again.
	r.pool.observe(kubeSTS("one"), kubernetes.Observation{Live: backend.LiveStatusRunning, Node: "n1", Holder: true})
	r.eventually("the cleared row", func() bool { return r.runtime("one").GetKube().GetMoveBlocked() == "" })
	r.pool.observe(kubeSTS("one"), cordoned())
	r.eventually("asked again", func() bool { return stub.count() == 2 })
	r.waitMoveEnded(stub)
}

// An instance the move stopped and could not start again is recorded
// stopped, why in its error and warning; one it could not stop only warned
// of.
func TestKubeEvacuationRecordsWhatDidNotStart(t *testing.T) {
	stub := &evacStub{failed: map[string]kubernetes.EvacuationOutcome{
		"two":   {Stopped: true, Err: errors.New("starting it again: kubernetes:timeout: pod did not become ready")},
		"three": {Err: errors.New("stopping it: pod still terminating")},
	}}
	r := newEvacRig(t, stub, kubeInstance("one", fleet.StatusRunning), kubeInstance("two", fleet.StatusRunning), kubeInstance("three", fleet.StatusRunning))
	r.waitTracked()
	r.pool.observe(kubeSTS("one"), cordoned())
	r.waitMoveEnded(stub)
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*fleet.Instance{}
	for _, inst := range st.Fleets["alpha"].Instances {
		byName[inst.Name] = inst
	}
	if one := byName["one"]; one.Status != fleet.StatusRunning || one.Error != "" {
		t.Fatalf("one: %s %q", one.Status, one.Error)
	}
	two := byName["two"]
	if two.Status != fleet.StatusStopped || !strings.HasPrefix(two.Error, "starting it again: kubernetes:timeout") || !strings.Contains(two.Error, "moving the fleet off node n1") {
		t.Fatalf("two: %s %q", two.Status, two.Error)
	}
	if three := byName["three"]; three.Status != fleet.StatusRunning {
		t.Fatalf("three (did not stop): %s", three.Status)
	}
	for _, name := range []string{"two", "three"} {
		data, err := os.ReadFile(state.WarnPath("alpha", name))
		if err != nil || !strings.Contains(string(data), "moving the fleet off node n1") {
			t.Fatalf("%s's warning: %q, %v", name, data, err)
		}
	}
}

// A move cut short by fleetd's shutdown records nothing: the next daemon
// finishes it.
func TestKubeEvacuationCutShortRecordsNothing(t *testing.T) {
	stub := &evacStub{failed: map[string]kubernetes.EvacuationOutcome{
		"one": {Stopped: true, Err: errors.New("starting it again: the Kubernetes pool is closed")},
	}}
	r := newEvacRig(t, stub, kubeInstance("one", fleet.StatusRunning))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.svc.kubePods.evac.run(ctx, evacKey{"c1", "alpha"}, "n1", "cordoned")
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if inst := st.Fleets["alpha"].Instances[0]; inst.Status != fleet.StatusRunning || inst.Error != "" {
		t.Fatalf("after a move cut short: %s %q", inst.Status, inst.Error)
	}
}

// A move a previous daemon began (the mark on a running record's
// StatefulSet, its pod gone) is finished; a mark left on an instance that
// runs elsewhere is not a move.
func TestKubeEvacuationFinishesAMoveBegunBefore(t *testing.T) {
	stub := &evacStub{}
	r := newEvacRig(t, stub, kubeInstance("one", fleet.StatusRunning))
	r.waitTracked()
	r.pool.observe(kubeSTS("one"), kubernetes.Observation{Live: backend.LiveStatusRunning, Node: "n2", Holder: true, MovingOff: "n1"})
	time.Sleep(30 * time.Millisecond)
	if n := stub.count(); n != 0 {
		t.Fatalf("a lingering mark moved the fleet (%d)", n)
	}
	r.pool.observe(kubeSTS("one"), kubernetes.Observation{Live: backend.LiveStatusStopped, MovingOff: "n1"})
	r.eventually("the move finished", func() bool { return stub.count() == 1 })
	if e := stub.last(); e.Node != "n1" || e.Why != "the daemon restarted during a move" {
		t.Fatalf("move %+v", e)
	}
	r.waitMoveEnded(stub)
}

// The members are the fleet's running instances on the connection whose
// record puts their share in single-node mode.
func TestKubeEvacuationMembersFromTheRecords(t *testing.T) {
	isolateFleetDir(t)
	one, two, three, four := kubeInstance("one", fleet.StatusRunning), kubeInstance("two", fleet.StatusRunning), kubeInstance("three", fleet.StatusStopped), kubeInstance("four", fleet.StatusRunning)
	two.KubeKey, three.KubeKey, four.KubeKey = "bcdefghi", "cdefghij", "defghijk"
	other := kubeInstance("elsewhere", fleet.StatusRunning)
	other.KubeCluster, other.KubeKey = "c2", "efghijkl"
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{one, two, three, four, other}}}}); err != nil {
		t.Fatal(err)
	}
	save := func(inst *fleet.Instance, share *kubernetes.ShareRecord) {
		rec := kubernetes.InstanceRecord{Cluster: inst.KubeCluster, Namespace: "ns", Key: inst.KubeKey, Fleet: "alpha", Instance: inst.Name,
			StatefulSet: names.Instance("alpha", inst.Name, inst.KubeKey), Gen: 1, Share: share}
		if err := kubernetes.SaveInstanceRecord(rec); err != nil {
			t.Fatal(err)
		}
	}
	single := &kubernetes.ShareRecord{Mode: string(topology.SingleNode), Claim: "fs-alpha-share"}
	save(one, single)
	save(two, &kubernetes.ShareRecord{Mode: string(topology.Copy)})
	save(three, single)
	save(four, nil)
	save(other, single)
	e := newKubeEvacuations(newKubePods(newService()))
	share, ms := e.members("c1", "alpha")
	if share != "fs-alpha-share" || len(ms) != 1 || ms[0].name != "one" || ms[0].sts != names.Instance("alpha", "one", kubeTestKey) || ms[0].key != kubeTestKey {
		t.Fatalf("members %q %+v", share, ms)
	}
	if !recordRunning("alpha", "one", kubeTestKey) || recordRunning("alpha", "one", "zzzzzzzz") || recordRunning("alpha", "three", "cdefghij") {
		t.Fatal("recordRunning")
	}
	if recordStopped("alpha", "one", "zzzzzzzz", "x") {
		t.Fatal("recorded another key's instance stopped")
	}
}
