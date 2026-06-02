package server

import (
	"context"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
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
	out := "main:2:1\nbg:1:0\n\nbad\n"
	sessions := parseTmuxSessionsProto(out)
	if len(sessions) != 3 {
		t.Fatalf("want 3 sessions (main, bg, bad), got %d", len(sessions))
	}
	if sessions[0].GetName() != "main" || sessions[0].GetWindows() != 2 || !sessions[0].GetAttached() {
		t.Fatalf("main parsed wrong: %+v", sessions[0])
	}
	if sessions[1].GetAttached() {
		t.Fatalf("bg should be unattached: %+v", sessions[1])
	}
	if sessions[2].GetName() != "bad" || sessions[2].GetWindows() != 0 {
		t.Fatalf("bad (name-only) parsed wrong: %+v", sessions[2])
	}
}
