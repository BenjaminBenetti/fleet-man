package server

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// service implements fleetgrpc.FleetServiceServer. Phase 1 implements only the
// read/lifecycle subset (Hello, GetState, Shutdown); every other RPC returns
// codes.Unimplemented via the embedded UnimplementedFleetServiceServer and
// lands in later phases.
type service struct {
	fleetgrpc.UnimplementedFleetServiceServer

	startedAt time.Time
	hub       *hub

	// muWrite serializes the synchronous state mutations (mutations.go) so two
	// concurrent mutation RPCs can't lost-update each other through the
	// load→apply→save cycle. This is a server-scoped fix for the issue #63 race
	// class; the full authoritative in-memory model (which removes the disk
	// round-trip entirely) lands in Phase 4. config.json writes (config.go) share
	// the same lock.
	muWrite sync.Mutex

	shutdownOnce sync.Once
	shutdownCh   chan struct{}
}

func newService() *service {
	return &service{startedAt: time.Now(), hub: newHub(), shutdownCh: make(chan struct{})}
}

// Hello is the authoritative version handshake. It reports the server's
// compiled-in version plus host-local liveness hints (pid, start time).
func (s *service) Hello(_ context.Context, _ *fleetgrpc.HelloRequest) (*fleetgrpc.HelloReply, error) {
	return &fleetgrpc.HelloReply{
		ServerVersion: version.Version,
		Pid:           int64(os.Getpid()),
		StartedAt:     timestamppb.New(s.startedAt),
	}, nil
}

// GetState returns the full snapshot the TUI/CLI render.
//
// Phase 1 is deliberately NON-CACHING: it re-Loads state.json from disk on every
// call rather than holding an authoritative in-memory model. Legacy writers
// (cli up/down/... ) still mutate state.json directly until Phase 4, so a cached
// snapshot here could clobber or hide their writes — a re-read can't. The
// authoritative in-memory model (and the actual #63 fix) arrive in Phase 4 when
// the last client-side writer is removed. Runtime + active_jobs are empty until
// Phase 2 (Watch/pollers) and Phase 4 (jobs).
func (s *service) GetState(_ context.Context, _ *fleetgrpc.GetStateRequest) (*fleetgrpc.GetStateReply, error) {
	st, err := state.Load()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load state: %v", err)
	}
	return &fleetgrpc.GetStateReply{State: stateToProto(st)}, nil
}

// Shutdown asks the server to stop. Phase 1 has no jobs, so a drain is
// immediate. The serve loop watches shutdownCh; GracefulStop there waits for
// THIS RPC to return before tearing the server down, so the caller still gets
// its reply.
func (s *service) Shutdown(_ context.Context, _ *fleetgrpc.ShutdownRequest) (*fleetgrpc.ShutdownReply, error) {
	s.shutdownOnce.Do(func() { close(s.shutdownCh) })
	return &fleetgrpc.ShutdownReply{Accepted: true, DrainingJobs: 0}, nil
}
