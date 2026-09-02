package server

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/server/remote"
	"github.com/BenjaminBenetti/fleet-man/internal/server/sshtunnel"
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
	jobs      *jobManager

	// remote drives the outbound remote-MCP gateway tunnel. Set in server.go
	// after the MCP listener binds (so it knows the loopback port); nil for tests
	// that use newService() without a serve loop, so callers must nil-check.
	remote *remote.Manager

	// sshListen serves the token-gated gRPC server on a loopback port while
	// remote fleet is enabled in SSH mode (sshlisten.go); sshTunnels maintains
	// this machine's OUTBOUND ssh forwards to ssh:// armada remotes
	// (ResolveArmadaRemote). Both set in server.go; nil in newService() tests.
	sshListen  *sshListener
	sshTunnels *sshtunnel.Manager

	// bgCtx is cancelled at server shutdown (set to the serve loop's hubCtx in
	// server.go). Background work spawned by RPC handlers — e.g. the TUI-connect
	// buildkit reconcile — derives from it so it stops promptly on shutdown
	// rather than orphaning. Defaults to context.Background() for tests that use
	// newService() without a running serve loop.
	bgCtx context.Context

	// muWrite serializes config.json writes (config.go SetConfig), which have no
	// package-level lock of their own. State.json mutations no longer use this —
	// they go through state.Update (mutations.go + the provisioning jobs), whose
	// package lock serializes every state writer in the process (the issue #63
	// fix). The two files are independent, so separate locks are fine.
	muWrite sync.Mutex

	shutdownOnce sync.Once
	shutdownCh   chan struct{}

	// buildkitReconciling coalesces the TUI-connect buildkit re-ensure so that
	// several TUIs opening at once trigger one sweep, not one per client.
	buildkitReconciling atomic.Bool

	// triggerFires carries matched trigger fires from a concurrent producer — the
	// webhook receiver or a bash trigger's probe goroutine — to the single-
	// goroutine scheduler, which spawns + watches the agents. A webhook request
	// sends ALL its matched triggers as one batch, so it enqueues all-or-nothing —
	// a sender that retries after a 503 can't double-fire triggers that already
	// enqueued; a bash probe sends a single-fire batch. Buffered so a burst doesn't
	// block the producer; a full channel makes the webhook receiver shed (503)
	// rather than block (see webhook.go). Drained only while runScheduler is
	// running (the real serve loop); tests that use newService() read it directly.
	triggerFires chan []triggerFire
}

func newService() *service {
	return &service{
		startedAt:    time.Now(),
		hub:          newHub(),
		jobs:         newJobManager(),
		shutdownCh:   make(chan struct{}),
		bgCtx:        context.Background(),
		triggerFires: make(chan []triggerFire, triggerFireBuffer),
	}
}

// reconcileTimeout bounds the TUI-connect buildkit reconcile so a slow/wedged
// docker daemon can't keep the coalescing flag held indefinitely (which would
// lock out future reconciles). It also frees the flag on shutdown via bgCtx.
const reconcileTimeout = 3 * time.Minute

// Hello is the authoritative version handshake. It reports the server's
// compiled-in version plus host-local liveness hints (pid, start time).
func (s *service) Hello(_ context.Context, _ *fleetgrpc.HelloRequest) (*fleetgrpc.HelloReply, error) {
	return &fleetgrpc.HelloReply{
		ServerVersion: version.Version,
		Pid:           int64(os.Getpid()),
		StartedAt:     timestamppb.New(s.startedAt),
	}, nil
}

// FleetTUIConnected is sent once when a TUI opens (CLI commands never send it).
// It kicks off fire-and-forget, once-per-open state reconciliation and returns
// immediately — the client does not wait on the outcome.
func (s *service) FleetTUIConnected(_ context.Context, _ *fleetgrpc.FleetTUIConnectedRequest) (*fleetgrpc.FleetTUIConnectedReply, error) {
	s.onTUIConnected()
	return &fleetgrpc.FleetTUIConnectedReply{}, nil
}

// onTUIConnected runs the once-per-connect, fire-and-forget reconciliation in a
// background goroutine. Today it re-ensures configured shared buildkit servers
// (recovering from an external kill / reboot).
//
// Coalesced via an atomic flag so N TUIs connecting simultaneously run the sweep
// once, not N times. NOTE the in-flight sweep reflects state as of WHEN IT
// STARTED: a setting toggled mid-sweep is not picked up by a coalesced late
// arrival until the sweep finishes and a subsequent TUI connects. That's
// acceptable here — it mirrors the rest of the feature, where a settings change
// only takes effect on the next instance create/clone/start.
//
// The flag is released when the sweep completes OR when reconcileTimeout elapses
// OR on shutdown (bgCtx) — released even if a docker call inside the sweep is
// wedged, so a broken daemon can never permanently lock out future reconciles
// (the inner sweep goroutine may then outlive this one, but it dies with the
// process and does no harm).
func (s *service) onTUIConnected() {
	if !s.buildkitReconciling.CompareAndSwap(false, true) {
		return // a reconcile is already in flight; it covers this connect
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.bgCtx, reconcileTimeout)
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			ensureConfiguredBuildkitServers(ctx)
			ensureConfiguredDebCacheServers(ctx)
			ensureConfiguredImageCacheServers(ctx)
		}()
		select {
		case <-done:
		case <-ctx.Done(): // timeout or shutdown
		}
		s.buildkitReconciling.Store(false)
	}()
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
	return &fleetgrpc.GetStateReply{State: protoconv.StateToProto(st), ActiveJobs: s.jobs.summaries()}, nil
}

// Shutdown asks the server to stop. Phase 1 has no jobs, so a drain is
// immediate. The serve loop watches shutdownCh; GracefulStop there waits for
// THIS RPC to return before tearing the server down, so the caller still gets
// its reply.
func (s *service) Shutdown(_ context.Context, _ *fleetgrpc.ShutdownRequest) (*fleetgrpc.ShutdownReply, error) {
	s.shutdownOnce.Do(func() { close(s.shutdownCh) })
	return &fleetgrpc.ShutdownReply{Accepted: true, DrainingJobs: 0}, nil
}
