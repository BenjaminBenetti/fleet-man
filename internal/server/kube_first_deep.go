//go:build !nok8s

package server

import (
	"context"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
)

// kube_first_deep.go runs a connection's deep test lazily, before its first
// fleet share is made (the plan's "deep test on the first create"). Without
// one, nothing has told whether two pods on one node can mount one
// ReadWriteOnce volume (share_pods), so a cluster where they cannot (the copy
// tier) would get single-node shares its second instance can never mount.
// The create of that share waits for the test, its checks shown as the
// create's step (kubernetes.Environment.DeepFacts), and the share takes the
// mode the test's result gives. Concurrent creates wait for the same run;
// once one ran for the connection (or a deep test from the editor or `fleet
// cluster test --deep` did, or one whose word the storage test cache keeps
// from before a restart), later creates take the facts at once. A run that
// could not tell is not run again by a create before the daemon restarts or
// the connection is re-pointed: the share falls back to single-node mode, as
// before.

// deepRun is a deep test a create started, which other creates join: the
// checks it reported so far (the latest of each) and who is listening.
type deepRun struct {
	done chan struct{}

	mu     sync.Mutex
	checks []*fleetgrpc.KubeCheck
	subs   map[int]func(*fleetgrpc.KubeCheck)
	next   int
}

func newDeepRun() *deepRun {
	return &deepRun{done: make(chan struct{}), subs: map[int]func(*fleetgrpc.KubeCheck){}}
}

// report passes c on to every listener and keeps it for the ones to come.
func (d *deepRun) report(c *fleetgrpc.KubeCheck) {
	d.mu.Lock()
	defer d.mu.Unlock()
	replaced := false
	for i, have := range d.checks {
		if have.GetName() == c.GetName() {
			d.checks[i], replaced = c, true
		}
	}
	if !replaced {
		d.checks = append(d.checks, c)
	}
	for _, f := range d.subs {
		f(c)
	}
}

// listen passes f the checks reported so far, then each one to come, until
// the returned func stops it.
func (d *deepRun) listen(f func(*fleetgrpc.KubeCheck)) func() {
	if f == nil {
		return func() {}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.checks {
		f(c)
	}
	id := d.next
	d.next++
	d.subs[id] = f
	return func() {
		d.mu.Lock()
		delete(d.subs, id)
		d.mu.Unlock()
	}
}

// deepTold reports whether a deep test told the connection's share mode
// already: one ran (deepAt), or its two-pod probe's word is known (the
// worker's, or the storage test cache's from before a restart).
func (w *kubeWorker) deepToldLocked() bool {
	if !w.deepAt.IsZero() || w.multiPodRWO != nil {
		return true
	}
	return w.storage != nil && w.storage.MultiPodRWO() != nil
}

// backendDeepFacts is connection id's facts for a create about to make the
// first share of a fleet there (kubernetes.Environment.DeepFacts): its last
// full preflight's (backendFacts), after the deep test when none has told the
// share mode yet, which it starts or joins and waits for (within ctx),
// passing progress each of its checks.
func (m *kubeManager) backendDeepFacts(ctx context.Context, id string, progress func(*fleetgrpc.KubeCheck)) (kubernetes.Facts, error) {
	facts, err := m.backendFacts(ctx, id)
	if err != nil {
		return facts, err
	}
	w := m.worker(id)
	if w == nil {
		return kubernetes.Facts{}, kubeerr.New(kubeerr.ClusterMissing, "no Kubernetes connection %q", id)
	}
	w.mu.Lock()
	run := w.firstDeep
	if run == nil {
		key := kubeConnectionKey(w.rec)
		if w.deepToldLocked() || w.firstDeepKey == key {
			w.mu.Unlock()
			return facts, nil
		}
		run = newDeepRun()
		w.firstDeep, w.firstDeepKey = run, key
		go w.runFirstDeep(run)
	}
	w.mu.Unlock()
	stop := run.listen(deepStepProgress(progress))
	defer stop()
	select {
	case <-run.done:
	case <-ctx.Done():
		return kubernetes.Facts{}, kubeerr.New(kubeerr.Unreachable, "the deep test of connection %s did not finish: %v", w.recName(), context.Cause(ctx))
	}
	return m.backendFacts(ctx, id)
}

// runFirstDeep runs the deep test run stands for, as long as the worker
// serves the connection (it outlives the create that started it: the result
// stays the connection's), and lets its waiters go.
func (w *kubeWorker) runFirstDeep(run *deepRun) {
	defer func() {
		w.mu.Lock()
		w.firstDeep = nil
		w.mu.Unlock()
		close(run.done)
	}()
	flog.Info("kube: the deep test runs before the connection's first fleet share", "cluster", w.recID())
	res := w.runFullDeep(w.runContext(), run.report, fullRun{deep: true})
	mode := ""
	if res.Capabilities != nil {
		mode = res.Capabilities.GetShareMode()
	}
	if res.MultiPodRWO == nil {
		flog.Warn("kube: the deep test could not tell whether two pods on one node mount one volume; new fleet shares keep the mode the checks gave", "cluster", w.recID(), "mode", mode, "state", res.State.Display())
		return
	}
	flog.Info("kube: the deep test decided the connection's share mode", "cluster", w.recID(), "mode", mode, "multi_pod_rwo", *res.MultiPodRWO)
}

// runContext is the worker's serve context, or (a worker the serve loop does
// not run) one of its own.
func (w *kubeWorker) runContext() context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx != nil {
		return w.ctx
	}
	return context.Background()
}

func (w *kubeWorker) recID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rec.ID
}

func (w *kubeWorker) recName() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rec.GetDisplayName()
}

// deepStepChecks are the deep test's own checks, which a create shows as its
// step (the rest of the full run is the preflight it had already).
var deepStepChecks = map[string]bool{
	preflight.CheckSharePods: true, preflight.CheckSharedStorage: true, preflight.CheckImagePull: true,
	preflight.CheckNetworkPolicy: true, preflight.CheckMetadata: true,
}

// deepStepProgress is progress for the deep test's own checks alone.
func deepStepProgress(progress func(*fleetgrpc.KubeCheck)) func(*fleetgrpc.KubeCheck) {
	if progress == nil {
		return nil
	}
	return func(c *fleetgrpc.KubeCheck) {
		if deepStepChecks[c.GetName()] {
			progress(c)
		}
	}
}
