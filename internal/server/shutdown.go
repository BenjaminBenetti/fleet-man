package server

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// shutdown.go is what a daemon that stopped serving still owes the instances
// before it exits (the bounded shutdown of PR #259, extended): in order, each
// step with a deadline of its own,
//
//  1. stop accepting PodExec and IssueKubeExecCredential (beginShutdown:
//     the shim on this host goes to the kubeconfig while fleetd is away);
//  2. flush the shared-state sync (5s);
//  3. send Close to the pods' agent and bulk sessions (2s);
//  4. close the port-forward pool and the appPort listeners;
//  5. cancel the informers and checkpoint the running Kubernetes jobs: each
//     stops at its next step and leaves its journal for the next start.
//
// Steps 2 to 5 share one budget (shutdownBudget) and run alongside the gRPC
// drain (stopGRPC), not after it, so all of it fits well inside the hard
// limit (shutdownHardLimit). The Shutdown RPC reports that limit, and a
// restarting client waits it out (plus 2s) before it kills the process, so a
// flush in progress is never killed and a wedged daemon still exits on its
// own. Every job is cancelled before the steps start (the serve loop cancels
// its hub context first), so a job winds down while they run.

var (
	// shutdownBudget bounds the shutdown steps together (vars so tests can
	// shorten them).
	shutdownBudget = 8 * time.Second
	// shareFlushLimit bounds the shared-state flush, sessionCloseLimit the
	// agent sessions' close.
	shareFlushLimit   = 5 * time.Second
	sessionCloseLimit = 2 * time.Second
	// stepGrace is how long past its deadline a step that does not heed its
	// context is waited for before the next one starts without it.
	stepGrace = 250 * time.Millisecond
	// shareFlush, when set (tests), stands in for the shared-state sync's
	// flush, which delivers what the sync still holds (agent logins,
	// settings, custom mounts) both ways before the pods' sessions close,
	// then closes it (shareSync.flushAndClose). Neither may depend on the hub
	// context, which is gone.
	shareFlush func(ctx context.Context) error
)

// shutdownStep is one step of the bounded shutdown.
type shutdownStep struct {
	name string
	// limit is the step's own deadline; 0 is what is left of the budget.
	limit time.Duration
	run   func(ctx context.Context)
}

// shutdownSteps are steps 2 to 5, in order.
func (s *service) shutdownSteps() []shutdownStep {
	steps := []shutdownStep{{name: "flush the shared state", limit: shareFlushLimit, run: func(ctx context.Context) {
		flush := shareFlush
		if flush == nil && s.shareSync != nil {
			flush = s.shareSync.flushAndClose
		}
		if flush != nil {
			if err := flush(ctx); err != nil {
				flog.Warn("fleet server shutdown: flushing the shared state", "err", err)
			}
		}
	}}}
	return append(steps, s.kubeShutdownSteps()...)
}

// runShutdownSteps runs steps in order within budget. Each gets a context
// that ends at its own limit or at the end of the budget, whichever is first;
// the next starts once it returns, or stepGrace past its deadline for one that
// does not heed its context (it is left running). Every step runs, so one the
// budget left no time for still does what it can at once.
func runShutdownSteps(budget time.Duration, steps []shutdownStep) {
	start := time.Now()
	end := start.Add(budget)
	for _, step := range steps {
		deadline := end
		if step.limit > 0 {
			if d := time.Now().Add(step.limit); d.Before(deadline) {
				deadline = d
			}
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		began := time.Now()
		done := make(chan struct{})
		go func() {
			defer close(done)
			step.run(ctx)
		}()
		timer := time.NewTimer(time.Until(deadline) + stepGrace)
		select {
		case <-done:
		case <-timer.C:
			flog.Warn("fleet server shutdown: a step outlived its deadline; going on without it", "step", step.name, "after", time.Since(began))
		}
		timer.Stop()
		cancel()
		if ms := flog.MillisSince(began); ms >= 100 {
			flog.Info("fleet server shutdown step", "step", step.name, "ms", ms)
		}
	}
	flog.Info("fleet server shutdown steps done", "ms", flog.MillisSince(start))
}

// beginShutdown is step 1: from here on the daemon-host shim's RPCs are
// refused (shuttingDown), so a pane resuming meanwhile reaches its pod with
// the kubeconfig instead, as while fleetd is down.
func (s *service) beginShutdown() { s.stopping.Store(true) }

// shuttingDown refuses an RPC that the shutdown stopped accepting.
func (s *service) shuttingDown() error {
	if s.stopping.Load() {
		return status.Error(codes.Unavailable, "fleet server is shutting down")
	}
	return nil
}
