//go:build !nok8s

package server

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_logs.go saves a Kubernetes instance's container output the way
// `docker logs` keeps a container's across stop/start: as the pool's
// informers see a run end that the cluster will drop — a restart (the
// previous container), a terminating pod, a crash loop about to be parked —
// fleetd reads that run's log from the API server and appends it to
// ~/.fleet/logs/<fleet>/<instance>/container.log, capped, each run once (by
// its pod UID and restart count). `fleet kube-exec logs`
// then prints the file before the live stream. The file is removed with the
// instance (the backend's teardown). A node failure, where the API server
// has no log, is covered by pod-init's own rolling copy on the instance
// disk, read back on the next start (readBackRolling).

// kubeContainerLogReader reads a container's ending log (the pool's).
type kubeContainerLogReader interface {
	ReadContainerLog(ctx context.Context, clusterID, pod string, previous bool, w io.Writer) error
}

// kubeLogs saves run-end output to container.log.
type kubeLogs struct {
	pool func() kubeContainerLogReader

	mu sync.Mutex
	// claimed is the runs (kubernetes.RunKey) a save is under way for or has
	// done in this process: a run's two writers (its end seen by the
	// informers, the rolling read-back) and its several end events (a park
	// and then the pod terminating) save it once. container.log's own
	// headers (kubernetes.SavedRuns) carry the same across fleetd restarts.
	claimed map[string]bool
}

func newKubeLogs() *kubeLogs {
	return &kubeLogs{
		pool:    func() kubeContainerLogReader { return kubernetes.DefaultPool() },
		claimed: map[string]bool{},
	}
}

// claim takes key for one save unless it is under way, done, or (saved)
// already in the file. release gives it back after a save that failed.
func (l *kubeLogs) claim(key string, saved func() map[string]bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.claimed[key] {
		return false
	}
	l.claimed[key] = true
	if saved()[key] {
		return false
	}
	return true
}

func (l *kubeLogs) release(key string) {
	l.mu.Lock()
	delete(l.claimed, key)
	l.mu.Unlock()
}

// onRunEnded appends the run's output to the instance's container.log. A
// restart saves the previous container's log; a terminating or parked pod the
// current one's. A run is saved once, whichever of its end events comes
// first; one whose log could not be read is left for a later event or the
// rolling read-back. ctx bounds the read.
func (l *kubeLogs) onRunEnded(ctx context.Context, e kubernetes.RunEnd) {
	fleetName, instance, ok := kubeInstanceOf(e.Cluster, e.StatefulSet)
	if !ok {
		return
	}
	key := kubernetes.RunKey(string(e.PodUID), e.RestartCount)
	if !l.claim(key, func() map[string]bool { return kubernetes.SavedRuns(fleetName, instance) }) {
		return
	}
	previous := e.Why == kubernetes.RunRestarted
	var buf bytes.Buffer
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := l.pool().ReadContainerLog(rctx, e.Cluster, e.Pod, previous, &buf)
	cancel()
	if err != nil || buf.Len() == 0 {
		// A terminating pod's log may already be gone, or the previous
		// container's was never kept: best effort, the live log and the
		// rolling copy remain. Not logged (it is routine on a stop).
		l.release(key)
		return
	}
	header := kubernetes.RunHeader(string(e.Why), string(e.PodUID), e.RestartCount, time.Now())
	if err := appendRun(fleetName, instance, append([]byte(header), buf.Bytes()...)); err != nil {
		l.release(key)
		flog.Warn("kube logs: appending to the saved container log", "fleet", fleetName, "instance", instance, "err", err)
	}
}

// rollingReader reads back pod-init's rolling copy of an instance's output
// (the Kubernetes backend handle).
type rollingReader interface {
	ReadRollingLog(ctx context.Context) ([]podinit.RunOutput, error)
}

// readBackRolling saves the ended runs of pod-init's rolling copy that are
// not saved yet — those whose API-server log was out of reach (a node that
// failed, a pod gone before its log was read, a run that ended while fleetd
// was down). It is the recovery the plan names: the next start (Fleet's, or
// one the cluster made) appends the part not yet saved. The current run is
// on the live log and is left out. Best effort.
func (l *kubeLogs) readBackRolling(ctx context.Context, fleetName, instance string, r rollingReader) {
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	runs, err := r.ReadRollingLog(rctx)
	if err != nil {
		flog.Info("kube logs: could not read back the instance's rolling log", "fleet", fleetName, "instance", instance, "err", err)
		return
	}
	ended := kubernetes.EndedRollingRuns(runs)
	if len(ended) == 0 {
		return
	}
	saved := kubernetes.SavedRuns(fleetName, instance)
	for _, run := range ended {
		if len(run.Data) == 0 {
			continue
		}
		key := kubernetes.RunKey(run.PodUID, int32(run.PodSeq))
		if !l.claim(key, func() map[string]bool { return saved }) {
			continue // saved from the API already (or being saved).
		}
		header := kubernetes.RunHeader("recovered", run.PodUID, int32(run.PodSeq), run.Start)
		if err := appendRun(fleetName, instance, append([]byte(header), run.Data...)); err != nil {
			l.release(key)
			flog.Warn("kube logs: appending a recovered run to the saved container log", "fleet", fleetName, "instance", instance, "err", err)
			return
		}
	}
}

// kubeInstanceOf is the fleet and instance whose StatefulSet is sts on
// connection clusterID; not one being torn down (its saved log goes with it).
func kubeInstanceOf(clusterID, sts string) (fleetName, instance string, ok bool) {
	st, err := state.Load()
	if err != nil {
		return "", "", false
	}
	for name, f := range st.Fleets {
		for _, inst := range f.Instances {
			if isKubeInstance(inst) && inst.KubeCluster == clusterID && names.Instance(name, inst.Name, inst.KubeKey) == sts {
				return name, inst.Name, inst.Status != fleet.StatusDeleting
			}
		}
	}
	return "", "", false
}

// kubeInstanceKept reports whether the instance is still there and not being
// torn down.
func kubeInstanceKept(fleetName, instance string) bool {
	st, err := state.Load()
	if err != nil {
		return true // unknown: keep what was saved
	}
	f := st.Fleets[fleetName]
	if f == nil {
		return false
	}
	inst, err := f.GetInstance(instance)
	return err == nil && inst.Status != fleet.StatusDeleting
}

// appendRun appends one saved run to the instance's container.log. The
// teardown removes the file: a save that raced it (the instance went, or
// began going, while the run's log was read) removes it again, so a log
// never outlives its instance (or greets the next one of the name).
func appendRun(fleetName, instance string, data []byte) error {
	if err := kubernetes.AppendContainerLog(fleetName, instance, data); err != nil {
		return err
	}
	if !kubeInstanceKept(fleetName, instance) {
		kubernetes.RemoveContainerLog(fleetName, instance)
	}
	return nil
}
