//go:build !nok8s

package server

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes/kubeimage"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/preflight"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeshim"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_backend.go is the daemon's side of the Kubernetes backend handles
// (kubernetes.SetEnvironment): the image delivery they share, and each
// connection's facts from its last full preflight, which a create waits for
// when the connection has not been checked yet. It also collects each
// connection's images past their retention, once an hour, while this daemon
// holds the connection's owner Lease.

// Waits and cadences.
var (
	// kubeFactsWait bounds a create's wait for its connection's first full
	// preflight; kubeFactsPoll how often it looks.
	kubeFactsWait = 3 * time.Minute
	kubeFactsPoll = 500 * time.Millisecond
	// kubeImageGCInterval paces a connection's image collection.
	kubeImageGCInterval = time.Hour
)

// kubeShimDirAge is how old a docker shim directory must be for the sweep
// at start to take it for a dead daemon's.
const kubeShimDirAge = 24 * time.Hour

// sweepKubeShimDirs removes the stale docker shim directories.
func sweepKubeShimDirs(now time.Time) {
	if n := kubeshim.SweepStale(kubeShimDirAge, now); n > 0 {
		flog.Info("kube: removed docker shim directories a stopped daemon left", "count", n)
	}
}

// setBackendEnvironment gives the backend handles the daemon's image
// delivery (and the gateway image server, when it runs) and connection facts.
// configureKubePool hands the daemon's pool the connections (a seam for
// tests).
var configureKubePool = func(records []fleet.KubeCluster) { kubernetes.DefaultPool().Configure(records) }

func (m *kubeManager) setBackendEnvironment(im *kubeImages) {
	env := kubernetes.Environment{Images: im.manager, Facts: m.backendFacts}
	if im.gateway != nil {
		env.Gateway = im.gateway
	}
	kubernetes.SetEnvironment(env)
}

// backendFacts is connection id's last full preflight, as a create needs it:
// waited for (and asked for) when none has finished, refused with the
// connection's state when it is not usable.
func (m *kubeManager) backendFacts(ctx context.Context, id string) (kubernetes.Facts, error) {
	ctx, cancel := context.WithTimeout(ctx, kubeFactsWait)
	defer cancel()
	asked := false
	for {
		w := m.worker(id)
		if w == nil {
			return kubernetes.Facts{}, kubeerr.New(kubeerr.ClusterMissing, "no Kubernetes connection %q", id)
		}
		w.mu.Lock()
		have, full, rec := w.haveFull, w.full, w.rec
		if !have && !asked {
			w.fullWanted = true
		}
		w.mu.Unlock()
		if have {
			if !preflight.Usable(full.State) {
				return kubernetes.Facts{}, kubeStateError(rec, full)
			}
			return kubernetes.Facts{Local: full.Local, Platform: full.Platform, Capabilities: full.Capabilities}, nil
		}
		if !asked {
			w.poke()
			asked = true
		}
		select {
		case <-ctx.Done():
			return kubernetes.Facts{}, kubeerr.New(kubeerr.Unreachable, "connection %s has not been checked yet: %v", rec.GetDisplayName(), context.Cause(ctx))
		case <-time.After(kubeFactsPoll):
		}
	}
}

// kubeStateError is why a connection is unusable, with the code of its state.
func kubeStateError(rec fleet.KubeCluster, res preflight.Result) error {
	code := kubeerr.Unreachable
	switch res.State {
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_UNAUTHORIZED:
		code = kubeerr.Unauthorized
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_FORBIDDEN:
		code = kubeerr.Forbidden
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_CONTEXT_MISSING,
		fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_PLUGIN_MISSING,
		fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_MISCONFIGURED:
		code = kubeerr.Misconfigured
	case fleetgrpc.KubeClusterState_KUBE_CLUSTER_STATE_OWNER_CONFLICT:
		code = kubeerr.OwnerConflict
	}
	msg := strings.TrimSpace(res.Message)
	if msg == "" {
		msg = res.State.Display()
	}
	return kubeerr.New(code, "connection %s is %s: %s", rec.GetDisplayName(), res.State.Display(), msg)
}

// kubeImageGC is when each connection's images were last collected.
type kubeImageGC struct {
	mu   sync.Mutex
	last map[string]time.Time
	busy map[string]bool
}

// collectImages collects connection rec's images past their retention
// (kubeimage's GC, every image no instance of state.json holds), at most
// once per kubeImageGCInterval; the caller holds the connection's owner
// Lease. In the background, bounded.
func (m *kubeManager) collectImages(rec fleet.KubeCluster, link *kubeLink) {
	im := m.imagesOrNil()
	if im == nil || link == nil || rec.ID == "" {
		return
	}
	m.imageGC.mu.Lock()
	if m.imageGC.last == nil {
		m.imageGC.last, m.imageGC.busy = map[string]time.Time{}, map[string]bool{}
	}
	if m.imageGC.busy[rec.ID] || m.now().Sub(m.imageGC.last[rec.ID]) < kubeImageGCInterval {
		m.imageGC.mu.Unlock()
		return
	}
	m.imageGC.busy[rec.ID], m.imageGC.last[rec.ID] = true, m.now()
	m.imageGC.mu.Unlock()
	go func() {
		defer func() {
			m.imageGC.mu.Lock()
			delete(m.imageGC.busy, rec.ID)
			m.imageGC.mu.Unlock()
		}()
		id, err := m.identity()
		if err != nil {
			return
		}
		live, err := liveKubeKeys(rec.ID)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), kubeImageCleanupTime)
		defer cancel()
		t := kubeimage.Target{Cluster: &rec, DaemonID: id.DaemonID, Labels: pullSecretLabels(id.Owner(), rec.ID),
			Secrets: kubernetes.PullSecrets{Client: link.client, Namespace: rec.Namespace}}
		res, err := im.manager.GC(ctx, t, kubeimage.GCOptions{Live: func(holder string) bool { return live[holder] }})
		switch {
		case err != nil:
			flog.Warn("kube: collecting the connection's images", "cluster", rec.ID, "err", err)
		case len(res.Deleted) > 0 || len(res.Warnings) > 0:
			flog.Info("kube: collected the connection's images", "cluster", rec.ID, "deleted", len(res.Deleted), "kept", len(res.Kept), "warnings", strings.Join(res.Warnings, "; "))
		}
	}()
}

// liveKubeKeys are the keys of connection id's instances in state.json:
// the holders whose images stay.
func liveKubeKeys(id string) (map[string]bool, error) {
	st, err := state.Load()
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, f := range st.Fleets {
		for _, inst := range f.Instances {
			if inst.KubeCluster == id && names.ValidHash(inst.KubeKey) {
				live[inst.KubeKey] = true
			}
		}
	}
	return live, nil
}
