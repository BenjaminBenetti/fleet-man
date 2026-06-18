package server

import (
	"context"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// TestApplyRuntimeMergesFields verifies the field-merge invariant: a poller that
// sets only its own fields must not zero another poller's fields on the shared
// h.runtime[key] entry (review finding — a stats tick must not clobber the
// live_status a status tick set).
func TestApplyRuntimeMergesFields(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub()
	go h.run(ctx)

	// live-status-only update.
	h.post(func(h *hub) {
		h.applyRuntime([]runtimeUpdate{{"f", "i", func(r *fleetgrpc.InstanceRuntime) {
			r.LiveStatus = fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_RUNNING
		}}})
	})
	// stats-only update for the same instance.
	h.post(func(h *hub) {
		h.applyRuntime([]runtimeUpdate{{"f", "i", func(r *fleetgrpc.InstanceRuntime) {
			r.Stats = &fleetgrpc.ContainerStats{CpuMillicores: 5, MemoryMb: 128}
		}}})
	})

	type res struct {
		ls  fleetgrpc.LiveContainerStatus
		cpu float64
	}
	ch := make(chan res, 1)
	h.post(func(h *hub) {
		r := h.runtime["f/i"]
		ch <- res{r.GetLiveStatus(), r.GetStats().GetCpuMillicores()}
	})
	got := <-ch

	if got.ls != fleetgrpc.LiveContainerStatus_LIVE_CONTAINER_STATUS_RUNNING {
		t.Fatalf("live_status was clobbered by the stats update: %v", got.ls)
	}
	if got.cpu != 5 {
		t.Fatalf("stats not applied: cpu=%v", got.cpu)
	}
}

func TestParseTmuxSessionsProto(t *testing.T) {
	out := "main:2:1\nbg:1:0\nmulti:3:2\n\nbad\n"
	sessions := parseTmuxSessionsProto(out)
	if len(sessions) != 4 {
		t.Fatalf("want 4 sessions (main, bg, multi, bad), got %d", len(sessions))
	}
	if sessions[0].GetName() != "main" || sessions[0].GetWindows() != 2 || !sessions[0].GetAttached() {
		t.Fatalf("main parsed wrong: %+v", sessions[0])
	}
	if sessions[1].GetAttached() {
		t.Fatalf("bg should be unattached: %+v", sessions[1])
	}
	// session_attached is a client count: 2 attached clients -> attached.
	if !sessions[2].GetAttached() || sessions[2].GetWindows() != 3 {
		t.Fatalf("multi (2 clients) should be attached: %+v", sessions[2])
	}
	if sessions[3].GetName() != "bad" || sessions[3].GetWindows() != 0 {
		t.Fatalf("bad (name-only) parsed wrong: %+v", sessions[3])
	}
}

// TestBackendForCachesAndPrunes verifies the runtime pollers reuse one backend
// per container across ticks (so the devcontainer user-lookup cache survives)
// and that pruneBackends evicts containers no longer running.
func TestBackendForCachesAndPrunes(t *testing.T) {
	h := newHub()
	// state.Load() hands the pollers a fresh *fleet.Instance every tick, so the
	// cache must key on identity (backend type + ContainerID), not pointer.
	// aNextTick is a distinct value with the same identity as a — the next
	// tick's reload — and must resolve to the same cached backend.
	a := &fleet.Instance{Name: "a", Backend: fleet.BackendDevcontainer, ContainerID: "ca"}
	aNextTick := &fleet.Instance{Name: "a", Backend: fleet.BackendDevcontainer, ContainerID: "ca"}
	b := &fleet.Instance{Name: "b", Backend: fleet.BackendDevcontainer, ContainerID: "cb"}
	// Same ContainerID as a but a different backend type: a Coder workspace and a
	// Codespace can share a name, so this must NOT collide with a.
	coderSameID := &fleet.Instance{Name: "a", Backend: fleet.BackendCoder, ContainerID: "ca"}

	if h.backendFor(a) != h.backendFor(aNextTick) {
		t.Fatal("backendFor returned a fresh backend for the same instance across distinct values")
	}
	if h.backendFor(a) == h.backendFor(coderSameID) {
		t.Fatal("backendFor reused one backend for instances sharing a ContainerID but differing by backend type")
	}
	_ = h.backendFor(b)
	if len(h.backends) != 3 {
		t.Fatalf("want 3 cached backends, got %d", len(h.backends))
	}

	// Only a is still running this pass; b and the coder instance dropped out.
	h.pruneBackends([]string{backendCacheKey(a)})
	if _, ok := h.backends[backendCacheKey(a)]; !ok {
		t.Fatal("pruneBackends evicted a still-running instance's backend")
	}
	if _, ok := h.backends[backendCacheKey(b)]; ok {
		t.Fatal("pruneBackends kept a backend whose instance is no longer running")
	}
	if _, ok := h.backends[backendCacheKey(coderSameID)]; ok {
		t.Fatal("pruneBackends kept a backend whose instance is no longer running")
	}
}
