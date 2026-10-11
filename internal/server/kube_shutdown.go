//go:build !nok8s

package server

import (
	"context"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_shutdown.go is the Kubernetes part of the bounded shutdown
// (shutdown.go): the pods' agent and bulk sessions are told to close (the
// agents hold their resumable commands for a later session), the published
// ports stop listening and the port-forward pool closes with every forwarded
// connection, and once the running Kubernetes jobs reached their checkpoint
// the pool ends, with its informers and whatever clean-up is still talking to
// a cluster.

// kubeShutdowner is the pool's part in the shutdown (*kubernetes.Pool).
type kubeShutdowner interface {
	CloseSessions(ctx context.Context)
	ClosePortForwards()
	Close()
}

// portForwardCloseLimit bounds closing the port-forward pool and the
// published-port listeners (a var so tests can shorten it).
var portForwardCloseLimit = time.Second

// kubeShutdownPool is the pool the shutdown ends; a var so tests stand in
// for it.
var kubeShutdownPool = defaultKubeShutdownPool

func defaultKubeShutdownPool() kubeShutdowner { return kubernetes.DefaultPool() }

// renewKubePool gives a daemon served again in this process (tests do so) a
// live pool: the last one's shutdown ended the pool it had.
func renewKubePool() { kubernetes.RenewDefaultPool() }

// kubeShutdownSteps are steps 3 to 5.
func (s *service) kubeShutdownSteps() []shutdownStep {
	pool := kubeShutdownPool()
	return []shutdownStep{
		{name: "close the agent sessions", limit: sessionCloseLimit, run: pool.CloseSessions},
		{name: "close the port forwards", limit: portForwardCloseLimit, run: func(context.Context) {
			// No new connection is accepted on a published port, and every
			// forwarded connection ends with its pod's port-forward.
			s.kubePods.ports.close()
			pool.ClosePortForwards()
		}},
		{name: "checkpoint the Kubernetes jobs", run: func(ctx context.Context) {
			s.checkpointKubeJobs(ctx)
			pool.Close()
		}},
	}
}

// checkpointKubeJobs waits, within ctx, for the provisioning jobs of the
// Kubernetes records to stop. The shutdown cancelled them: each stops at its
// next step and leaves its journal for the next start, its record as it is
// (a create stays creating and is failed kubernetes:interrupted at its step,
// an undo of a rebuild stays rebuilding and is undone again). One still
// running when ctx ends is cut wherever it is when the pool ends; its
// journal, written before each of its cluster writes, still says what it did.
func (s *service) checkpointKubeJobs(ctx context.Context) {
	jobs := s.kubeProvisioningJobs()
	if len(jobs) == 0 {
		return
	}
	stuck := awaitJobs(ctx, jobs, shutdownBudget)
	for _, j := range stuck {
		flog.Warn("fleet server shutdown: a Kubernetes job did not reach its checkpoint in time; its journal stays for the next start",
			"job", j.summary.GetJobId(), "kind", jobKindName(j.summary.GetKind()), "fleet", j.summary.GetFleet(), "instance", j.summary.GetInstance())
	}
	flog.Info("fleet server shutdown: Kubernetes jobs checkpointed", "jobs", len(jobs), "cut", len(stuck))
}

// kubeProvisioningJobs are the in-flight provisioning jobs of Kubernetes
// records (every provisioning job when the records cannot be read).
func (s *service) kubeProvisioningJobs() []*job {
	jobs := s.jobs.allProvisioningJobs()
	if len(jobs) == 0 {
		return nil
	}
	st, err := state.Load()
	if err != nil {
		return jobs
	}
	var out []*job
	for _, j := range jobs {
		if inst := findInstance(st, j.summary.GetFleet(), j.summary.GetInstance()); inst != nil && inst.Backend == fleet.BackendKubernetes {
			out = append(out, j)
		}
	}
	return out
}
