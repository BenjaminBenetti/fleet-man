//go:build !nok8s

package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// fakeMover stands in for the pool's share moves.
type fakeMover struct {
	mu      sync.Mutex
	moves   map[fleetShare]*kubernetes.ShareMove
	moved   []fleetShare
	result  error
	release chan struct{}
}

func (f *fakeMover) ShareMoveOf(_ context.Context, cluster, fleetName string) (*kubernetes.ShareMove, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.moves[fleetShare{cluster, fleetName}]
	if m == nil {
		return nil, nil
	}
	c := *m
	return &c, nil
}

func (f *fakeMover) MoveShare(_ context.Context, cluster, fleetName string, report func(string)) error {
	report("copying the share to a nfs volume")
	if f.release != nil {
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moved = append(f.moved, fleetShare{cluster, fleetName})
	if f.result == nil {
		delete(f.moves, fleetShare{cluster, fleetName})
	}
	return f.result
}

func moveState(t *testing.T, statuses map[string]fleet.InstanceStatus) {
	t.Helper()
	f := &fleet.Fleet{Name: "alpha"}
	i := 0
	for name, s := range statuses {
		i++
		f.Instances = append(f.Instances, &fleet.Instance{Name: name, Backend: fleet.BackendKubernetes, KubeCluster: "c1", KubeKey: fmt.Sprintf("%08d", i), Status: s})
	}
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}); err != nil {
		t.Fatal(err)
	}
}

func newTestShareMoves(f *fakeMover) *kubeShareMoves {
	m := newKubeShareMoves(nil)
	m.pool = func() shareMover { return f }
	m.usable = func(context.Context, string) bool { return true }
	return m
}

// waitMoves waits for the moves m began to end.
func waitMoves(t *testing.T, m *kubeShareMoves) {
	t.Helper()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a move never ended")
	}
}

// TestKubeShareMoves: a fleet whose share is in another class than its
// connection's shared one waits while an instance runs there, and moves
// once all are stopped (moving while it does); then it is no longer listed.
// A blocked move says why; a failed one is tried again only after a while.
func TestKubeShareMoves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	f := &fakeMover{moves: map[fleetShare]*kubernetes.ShareMove{{"c1", "alpha"}: {Fleet: "alpha", From: "standard", To: "nfs"}}}
	m := newTestShareMoves(f)

	moveState(t, map[string]fleet.InstanceStatus{"web": fleet.StatusRunning, "api": fleet.StatusStopped})
	m.pass(ctx)
	got := m.status("c1")
	if len(got) != 1 || got[0].GetState() != shareMoveWaiting || got[0].GetDetail() != "waits for 1 running instance on the cluster to stop" || got[0].GetFromClass() != "standard" || got[0].GetToClass() != "nfs" {
		t.Fatalf("while running: %v", got)
	}
	if len(f.moved) != 0 {
		t.Fatal("moved a fleet that runs")
	}

	f.release = make(chan struct{})
	moveState(t, map[string]fleet.InstanceStatus{"web": fleet.StatusStopped, "api": fleet.StatusFailed})
	m.pass(ctx)
	if got := m.status("c1"); len(got) != 1 || got[0].GetState() != shareMoveMoving {
		t.Fatalf("moving: %v", got)
	}
	m.pass(ctx) // a pass meanwhile begins no second move
	close(f.release)
	waitMoves(t, m)
	if len(f.moved) != 1 {
		t.Fatalf("moved %v", f.moved)
	}
	if got := m.status("c1"); len(got) != 0 {
		t.Fatalf("after the move: %v", got)
	}
	if got := m.status("c2"); len(got) != 0 {
		t.Fatalf("another connection: %v", got)
	}

	// Failed: listed, and not tried again before shareMoveRetry.
	f.release, f.result = nil, errors.New("copy Job failed: no space left on device")
	f.moves[fleetShare{"c1", "alpha"}] = &kubernetes.ShareMove{Fleet: "alpha", From: "standard", To: "nfs"}
	now := time.Now()
	m.now = func() time.Time { return now }
	m.pass(ctx)
	waitMoves(t, m)
	if got := m.status("c1"); len(got) != 1 || got[0].GetState() != shareMoveFailed || !strings.Contains(got[0].GetDetail(), "no space left") {
		t.Fatalf("failed: %v", got)
	}
	m.pass(ctx)
	waitMoves(t, m)
	if len(f.moved) != 2 {
		t.Fatalf("a failed move was tried again at once: %v", f.moved)
	}
	now = now.Add(shareMoveRetry + time.Second)
	m.pass(ctx)
	waitMoves(t, m)
	if len(f.moved) != 3 {
		t.Fatalf("a failed move was not tried again: %v", f.moved)
	}

	// An instance started as it began: it waits for the next stop.
	f.result = fmt.Errorf("%w: instance web runs", kubernetes.ErrShareInUse)
	now = now.Add(shareMoveRetry + time.Second)
	m.pass(ctx)
	waitMoves(t, m)
	if got := m.status("c1"); len(got) != 1 || got[0].GetState() != shareMoveWaiting {
		t.Fatalf("in use: %v", got)
	}

	// The shared class changed since the last full preflight: waiting for
	// the test to judge it, never begun.
	f.moves[fleetShare{"c1", "alpha"}] = &kubernetes.ShareMove{Fleet: "alpha", From: "standard", To: "nfs2", Untested: "waits for the cluster test to judge nfs2"}
	n := len(f.moved)
	m.pass(ctx)
	if got := m.status("c1"); len(got) != 1 || got[0].GetState() != shareMoveWaiting || got[0].GetDetail() != "waits for the cluster test to judge nfs2" || len(f.moved) != n {
		t.Fatalf("untested: %v (moved %v)", got, f.moved)
	}

	// Blocked: listed with why, never begun.
	f.result = nil
	f.moves[fleetShare{"c1", "alpha"}] = &kubernetes.ShareMove{Fleet: "alpha", From: "nfs", To: "fast", Blocked: "its instances run on any node"}
	m.pass(ctx)
	if got := m.status("c1"); len(got) != 1 || got[0].GetState() != shareMoveBlocked || got[0].GetDetail() != "its instances run on any node" || len(f.moved) != n {
		t.Fatalf("blocked: %v (moved %v)", got, f.moved)
	}

	// The share is where it belongs: no longer listed.
	delete(f.moves, fleetShare{"c1", "alpha"})
	m.pass(ctx)
	if got := m.status("c1"); len(got) != 0 {
		t.Fatalf("where it belongs: %v", got)
	}
}

// The moves are on the connection's pushed status.
func TestKubeShareMovesOnTheClusterStatus(t *testing.T) {
	f := newKubeFixture(t)
	f.start()
	dev := f.add(&fleetgrpc.KubeCluster{Context: "minikube", DisplayName: "dev", Namespace: "fleet"}).GetCluster()
	f.svc.shareMoves.mu.Lock()
	f.svc.shareMoves.entries[fleetShare{dev.GetId(), "alpha"}] = &shareMoveEntry{from: "standard", to: "nfs", state: shareMoveWaiting, detail: "waits for 1 running instance on the cluster to stop"}
	f.svc.shareMoves.mu.Unlock()
	st := f.svc.kube.status(dev.GetId())
	if got := st.GetShareMigrations(); len(got) != 1 || got[0].GetFleet() != "alpha" || got[0].GetState() != shareMoveWaiting {
		t.Fatalf("share_migrations %v", got)
	}
}
