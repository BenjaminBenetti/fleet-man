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
	// mic is the virtual-microphone hub (mic.go): provider streams, per-instance
	// sinks, and the demand/audio routing between them. Its sync loop is started
	// by the serve loop; without it (newService() tests) the hub is inert.
	mic    *micHub
	output *outputHub
	// agent is the SSH-agent relay (sshagent.go): provider streams and the
	// relay sockets the daemon's children and instances connect to.
	agent       *agentHub
	gitHostKeys gitHostKeyHub
	// instanceMCP serves the fleet MCP server into the instances of fleets
	// with the Fleet MCP setting on (mcp_instance.go). Its sync loop is
	// started by the serve loop; without it (newService() tests) it is inert.
	instanceMCP *instanceMCP

	// remote drives the outbound remote-MCP gateway tunnel. Set in server.go
	// after the MCP listener binds (so it knows the loopback port); nil for tests
	// that use newService() without a serve loop, so callers must nil-check.
	remote *remote.Manager

	// kube runs the Kubernetes connections: their probes, owner Leases and
	// health push (kube_health.go; inert in a -tags nok8s build). The serve
	// loop starts it once fleetd serves; without it (newService() tests) the
	// kube RPCs still work and connections stay CHECKING.
	kube *kubeManager
	// kubePods drives the pool from fleetd's state and acts on what its
	// informers see of the Kubernetes instances' pods: restarts the cluster
	// made, crash loops, agent sessions (kube_pods.go; inert in a -tags nok8s
	// build). Started by the serve loop once fleetd serves.
	kubePods *kubePods
	// podAgents is the daemon's side of the Kubernetes pods' in-pod sockets:
	// what each pod binds and the connections they relay (podagent_hub.go;
	// inert in a -tags nok8s build). Started by the serve loop.
	podAgents *podAgentHub
	// control is the control-socket registry: the host sockets, and the
	// envelope handler the pods' relayed control connections share. Set by
	// the serve loop; nil in newService() tests.
	control *controlRegistry
	// kubeRecovery reconciles the Kubernetes jobs a previous daemon died in
	// and deletes known garbage (kube_reconcile.go; inert in a -tags nok8s
	// build). failInterruptedJobs uses its journals at start; the serve
	// loop starts its undo jobs and background passes.
	kubeRecovery *kubeRecovery
	// shareSync keeps each fleet's shared folders on its clusters converged
	// with the daemon host's (share_sync.go; inert in a -tags nok8s build,
	// and until a fleet has an instance on a cluster). Started by the serve
	// loop; flushed and closed by the shutdown (shareFlush).
	shareSync *shareSync

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
	// stopping is set as the shutdown begins (beginShutdown, shutdown.go).
	stopping atomic.Bool

	// sharedServicesReconciling coalesces the TUI-connect shared-service
	// re-ensure so that several TUIs opening at once trigger one sweep, not one
	// per client.
	sharedServicesReconciling atomic.Bool

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

	// automation is the automation limiter (automation_limits.go): the
	// cluster agents' limits and their triggers' queues, shared by the
	// scheduler and the webhook receiver; made on first use (limiter), so a
	// zero service in a test has one too.
	automation     *automationLimiter
	automationOnce sync.Once
	// schedulerWake asks the scheduler to look at its queues and status
	// again (a settings change) without waiting for its next tick.
	schedulerWake chan struct{}
}

// limiter is the service's automation limiter.
func (s *service) limiter() *automationLimiter {
	s.automationOnce.Do(func() {
		if s.automation == nil {
			s.automation = newAutomationLimiter()
		}
	})
	return s.automation
}

// wakeScheduler asks the running scheduler to look again at once; a no-op
// without one.
func (s *service) wakeScheduler() {
	if s.schedulerWake == nil {
		return
	}
	select {
	case s.schedulerWake <- struct{}{}:
	default:
	}
}

func newService() *service {
	svc := &service{
		startedAt:    time.Now(),
		hub:          newHub(),
		jobs:         newJobManager(),
		mic:          newMicHub(),
		output:       newOutputHub(),
		agent:        newAgentHub(),
		shutdownCh:   make(chan struct{}),
		bgCtx:        context.Background(),
		triggerFires: make(chan []triggerFire, triggerFireBuffer),
		// Buffered by one: a wake while the scheduler is busy is kept, more
		// are the same wake.
		schedulerWake: make(chan struct{}, 1),
	}
	svc.instanceMCP = newInstanceMCP(svc)
	svc.kube = newKubeManager(svc)
	svc.kubePods = newKubePods(svc)
	svc.podAgents = newPodAgentHub(svc)
	svc.kubeRecovery = newKubeRecovery(svc)
	svc.shareSync = newShareSync(svc)
	svc.hub.stateListeners = append(svc.hub.stateListeners, svc.kubePods.onState, svc.podAgents.onState, svc.shareSync.onState)
	// The in-flight jobs and their current steps are pushed over Watch. The
	// set is taken on the hub loop, so pushes land in the order they happen
	// and the last one is always current.
	svc.jobs.onChange = func() {
		svc.hub.post(func(h *hub) { h.broadcastActiveJobs(svc.jobs.activeJobs()) })
	}
	svc.output.configMu = &svc.muWrite
	svc.output.onChange = func(snapshot *fleetgrpc.OutputTargets) {
		svc.hub.post(func(h *hub) { h.broadcastOutputTargets(snapshot) })
	}
	// The attached microphone clients, and the settings that select among them,
	// are pushed over Watch. The mic hub calls this from a single goroutine, one
	// change at a time, so the snapshots reach the Watch hub in the order they
	// were taken.
	var lastSettings *fleetgrpc.MicSettings
	svc.mic.onSources = func() {
		snapshot := svc.micSourcesNow(lastSettings)
		lastSettings = snapshot.GetSettings()
		svc.hub.post(func(h *hub) { h.broadcastMicSources(snapshot) })
	}
	return svc
}

// reconcileTimeout bounds the TUI-connect shared-service reconcile so a
// slow/wedged runtime can't keep the coalescing flag held indefinitely (which
// would lock out future reconciles). It also frees the flag on shutdown via
// bgCtx.
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
// background goroutine. Today it re-ensures the fleets' configured shared
// services (recovering from an external kill / reboot; see
// ensureConfiguredSharedServices).
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
	if !s.sharedServicesReconciling.CompareAndSwap(false, true) {
		return // a reconcile is already in flight; it covers this connect
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.bgCtx, reconcileTimeout)
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			ensureConfiguredSharedServices(ctx)
		}()
		select {
		case <-done:
		case <-ctx.Done(): // timeout or shutdown
		}
		s.sharedServicesReconciling.Store(false)
	}()
}

// GetState returns the full snapshot the TUI/CLI render.
//
// Phase 1 is deliberately NON-CACHING: it re-Loads state.json from disk on every
// call rather than holding an authoritative in-memory model. Legacy writers
// (cli up/down/... ) still mutate state.json directly until Phase 4, so a cached
// snapshot here could clobber or hide their writes — a re-read can't. The
// authoritative in-memory model (and the actual #63 fix) arrive in Phase 4 when
// the last client-side writer is removed. The live runtime streams over Watch;
// here it carries only current_branch, read on demand when the caller sets
// fresh_branches (branches.go), since the runtime pollers run only while a TUI
// watches.
func (s *service) GetState(ctx context.Context, req *fleetgrpc.GetStateRequest) (*fleetgrpc.GetStateReply, error) {
	st, err := state.Load()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load state: %v", err)
	}
	reply := &fleetgrpc.GetStateReply{State: protoconv.StateToProto(st), ActiveJobs: s.jobs.summaries()}
	if req.GetFreshBranches() {
		reply.Runtime = s.hub.freshBranches(ctx, st)
	}
	return reply, nil
}

// Shutdown asks the server to stop. Running jobs are not waited for (the
// Kubernetes ones stop at a checkpoint, shutdown.go). The serve loop watches
// shutdownCh; GracefulStop there waits for THIS RPC to return before tearing
// the server down, so the caller still gets its reply, which says how long
// the shutdown may take before the daemon exits regardless (its hard limit):
// what a restarting client waits for before it kills the process.
func (s *service) Shutdown(_ context.Context, _ *fleetgrpc.ShutdownRequest) (*fleetgrpc.ShutdownReply, error) {
	s.shutdownOnce.Do(func() { close(s.shutdownCh) })
	return &fleetgrpc.ShutdownReply{Accepted: true, DrainingJobs: 0, HardLimitMs: shutdownHardLimit.Milliseconds()}, nil
}
