//go:build !nok8s

package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
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
// ~/.fleet/logs/<fleet>/<instance>/container.log, capped and deduped by pod
// UID and restart count so nothing is saved twice. `fleet kube-exec logs`
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

	mu        sync.Mutex
	saved     map[string]bool // podUID/restartCount/why already appended (onRunEnded idempotency)
	savedPods map[string]bool // pod UIDs whose output onRunEnded saved from the API server
}

func newKubeLogs() *kubeLogs {
	return &kubeLogs{
		pool:      func() kubeContainerLogReader { return kubernetes.DefaultPool() },
		saved:     map[string]bool{},
		savedPods: map[string]bool{},
	}
}

// onRunEnded appends the run's output to the instance's container.log. A
// restart saves the previous container's log; a terminating or parked pod the
// current one's. ctx bounds the read.
func (l *kubeLogs) onRunEnded(ctx context.Context, e kubernetes.RunEnd) {
	key := fmt.Sprintf("%s/%d/%s", e.PodUID, e.RestartCount, e.Why)
	l.mu.Lock()
	if l.saved[key] {
		l.mu.Unlock()
		return
	}
	l.saved[key] = true
	l.mu.Unlock()

	fleetName, instance, ok := kubeInstanceOf(e.Cluster, e.StatefulSet)
	if !ok {
		return
	}
	previous := e.Why == kubernetes.RunRestarted
	var buf bytes.Buffer
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := l.pool().ReadContainerLog(rctx, e.Cluster, e.Pod, previous, &buf)
	cancel()
	if err != nil {
		// A terminating pod's log may already be gone, or the previous
		// container's was never kept: best effort, the live log and the
		// rolling copy remain. Not logged (it is routine on a stop).
		return
	}
	if buf.Len() == 0 {
		return
	}
	header := fmt.Sprintf("===== %s run (pod %s restart %d) %s =====\n", e.Why, e.PodUID, e.RestartCount, time.Now().UTC().Format(time.RFC3339))
	if err := kubernetes.AppendContainerLog(fleetName, instance, append([]byte(header), buf.Bytes()...)); err != nil {
		flog.Warn("kube logs: appending to the saved container log", "fleet", fleetName, "instance", instance, "err", err)
		return
	}
	l.mu.Lock()
	l.savedPods[string(e.PodUID)] = true
	l.mu.Unlock()
}

// rollingReader reads back pod-init's rolling copy of an instance's output
// (the Kubernetes backend handle).
type rollingReader interface {
	ReadRollingLog(ctx context.Context) ([]podinit.RunOutput, error)
}

// readBackRolling saves the runs of pod-init's rolling copy that onRunEnded
// could not — the ones on a node that failed, whose API-server log was out of
// reach (their pod UID never reached savedPods). It is the node-failure
// recovery the plan names: the next start (a cluster-made restart's
// post-start) appends the part not yet saved. Best effort.
func (l *kubeLogs) readBackRolling(ctx context.Context, fleetName, instance string, r rollingReader) {
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	runs, err := r.ReadRollingLog(rctx)
	if err != nil {
		flog.Info("kube logs: could not read back the instance's rolling log", "fleet", fleetName, "instance", instance, "err", err)
		return
	}
	for _, run := range runs {
		if run.PodUID == "" || len(run.Data) == 0 {
			continue
		}
		l.mu.Lock()
		done := l.savedPods[run.PodUID]
		if !done {
			l.savedPods[run.PodUID] = true
		}
		l.mu.Unlock()
		if done {
			continue // onRunEnded already saved this run's output from the API.
		}
		header := fmt.Sprintf("===== recovered run (pod %s) %s =====\n", run.PodUID, run.Start.UTC().Format(time.RFC3339))
		if err := kubernetes.AppendContainerLog(fleetName, instance, append([]byte(header), run.Data...)); err != nil {
			flog.Warn("kube logs: appending a recovered run to the saved container log", "fleet", fleetName, "instance", instance, "err", err)
			return
		}
	}
}

// kubeInstanceOf is the fleet and instance whose StatefulSet is sts on
// connection clusterID.
func kubeInstanceOf(clusterID, sts string) (fleetName, instance string, ok bool) {
	st, err := state.Load()
	if err != nil {
		return "", "", false
	}
	for name, f := range st.Fleets {
		for _, inst := range f.Instances {
			if isKubeInstance(inst) && inst.KubeCluster == clusterID && names.Instance(name, inst.Name, inst.KubeKey) == sts {
				return name, inst.Name, true
			}
		}
	}
	return "", "", false
}
