//go:build !nok8s

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetlaunch"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/kubeerr"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// backend.go is the Kubernetes backend's handle on one instance: what
// backendutil.NewForInstance returns for a Kubernetes record. It is bound to
// the instance's identity (its connection, its key, its creation time, its
// fleet), from which every object of it is named, so it acts on the instance
// with or without a ContainerID. Making one does no I/O: pollers and
// capability checks under the state lock make them freely.
//
// The lifecycle (create, stop, start, down), the status, the agent's polls
// and scripts, the control channel and the fleet binary's staging are here.
// What reaches into the instance from outside fleetd's process (exec,
// ExecCommand, files, logs, stats, port forwards) goes through the daemon-host
// kube-exec shim, which comes next; until then those methods fail with
// kubernetes:unavailable naming what is missing.

// Backend is the handle on one Kubernetes instance.
type Backend struct {
	pool *Pool
	inst fleet.Instance

	fleetOnce sync.Once
	fleetName string
	fleetErr  error
}

// The interfaces the handle provides besides backend.Backend.
var (
	_ backend.UpContexter      = (*Backend)(nil)
	_ backend.RebuildContexter = (*Backend)(nil)
	_ backend.OrphanDowner     = (*Backend)(nil)
	_ backend.ControlChannel   = (*Backend)(nil)
	_ backend.RestartAware     = (*Backend)(nil)
	_ backend.PostStarter      = (*Backend)(nil)
	_ TargetProvider           = (*Backend)(nil)
	_ fleetlaunch.Stager       = (*Backend)(nil)
	_ backend.StreamExecer     = (*Backend)(nil)
	_ backend.TmuxEnsurer      = (*Backend)(nil)
)

// ForInstance is the handle on inst, a Kubernetes record. It keeps a copy
// of the record and does no I/O.
func ForInstance(inst *fleet.Instance) backend.Backend { return newHandle(inst, nil) }

// newHandle binds inst to pool (nil: the daemon's).
func newHandle(inst *fleet.Instance, pool *Pool) *Backend {
	b := &Backend{pool: pool}
	if inst != nil {
		b.inst = *inst
	}
	return b
}

func (b *Backend) p() *Pool {
	if b.pool != nil {
		return b.pool
	}
	return DefaultPool()
}

// bound is the instance's identity, as its handle names its objects.
type bound struct {
	cluster, key, fleet, name, sts string
	created                        time.Time
}

// errUnbound is a handle whose record does not say where the instance runs.
var errUnbound = errors.New("the instance record names no Kubernetes connection or key")

// identity is the bound instance's identity.
func (b *Backend) identity() (bound, error) {
	if b.inst.KubeCluster == "" || !names.ValidHash(b.inst.KubeKey) || b.inst.Name == "" {
		return bound{}, errUnbound
	}
	f, err := b.fleet()
	if err != nil {
		return bound{}, err
	}
	return bound{
		cluster: b.inst.KubeCluster, key: b.inst.KubeKey, fleet: f, name: b.inst.Name,
		sts:     names.Instance(f, b.inst.Name, b.inst.KubeKey),
		created: b.inst.CreatedAt,
	}, nil
}

// fleet is the bound instance's fleet: from its workspace path
// (<workspaces>/<fleet>/<instance>/<fleet>, as every create lays it out),
// else from state.json (the record holding the instance's key).
func (b *Backend) fleet() (string, error) {
	b.fleetOnce.Do(func() {
		if f, ok := fleetOfWorkspace(state.WorkspacesDir(), b.inst.WorkspaceDir, b.inst.Name); ok {
			b.fleetName = f
			return
		}
		st, err := state.Load()
		if err != nil {
			b.fleetErr = fmt.Errorf("finding the instance's fleet: %w", err)
			return
		}
		for name, f := range st.Fleets {
			for _, inst := range f.Instances {
				if inst.KubeCluster == b.inst.KubeCluster && inst.KubeKey == b.inst.KubeKey && inst.Name == b.inst.Name {
					b.fleetName = name
					return
				}
			}
		}
		b.fleetErr = fmt.Errorf("no fleet holds instance %s (key %s)", b.inst.Name, b.inst.KubeKey)
	})
	return b.fleetName, b.fleetErr
}

// fleetOfWorkspace is the fleet of a workspace dir laid out as
// <root>/<fleet>/<instance>/<fleet>.
func fleetOfWorkspace(root, wsDir, instance string) (string, bool) {
	if wsDir == "" || root == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, wsDir)
	if err != nil {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[0] == "" || parts[0] == ".." || parts[0] != parts[2] || parts[1] != instance {
		return "", false
	}
	return parts[0], true
}

// where is the instance a ContainerID names: its connection, namespace,
// StatefulSet and generation, from the ID when it parses, else from the
// bound record.
type where struct {
	cluster, namespace, sts string
	gen                     int
}

func (b *Backend) where(containerID string) (where, error) {
	if id, ok := fleet.ParseKubeContainerID(containerID); ok {
		return where{cluster: id.Cluster, namespace: id.Namespace, sts: id.StatefulSet, gen: id.Generation}, nil
	}
	if containerID != "" && containerID != b.inst.ContainerID {
		return where{}, fmt.Errorf("%q is not a Kubernetes container id", containerID)
	}
	id, err := b.identity()
	if err != nil {
		return where{}, err
	}
	w := where{cluster: id.cluster, sts: id.sts, gen: 1}
	if cid, ok := fleet.ParseKubeContainerID(b.inst.ContainerID); ok {
		w.namespace, w.gen = cid.Namespace, cid.Generation
	}
	return w, nil
}

// ContainerID is the ContainerID of generation gen of the instance whose
// StatefulSet sts runs in namespace ns on connection clusterID.
func ContainerID(clusterID, ns, sts string, gen int) string {
	return fleet.KubeContainerID{Cluster: clusterID, Namespace: ns, StatefulSet: sts, Generation: gen}.String()
}

// --- capabilities ---

// SupportsSharedMounts is true: a fleet's shared mounts are handed to the
// translation, which refuses (naming them) those the cluster cannot have yet
// rather than dropping them.
func (b *Backend) SupportsSharedMounts() bool { return true }

// SupportsClone is true (clone.go), SupportsRebuild too (rebuild.go);
// SupportsMicSink is audio.go's.
func (b *Backend) SupportsClone() bool   { return true }
func (b *Backend) SupportsRebuild() bool { return true }

// Up is UpContext without a job's context.
func (b *Backend) Up(workspaceDir string, mounts []backend.Mount) (*backend.UpResult, error) {
	return b.UpContext(context.Background(), backend.UpRequest{WorkspaceDir: workspaceDir, Mounts: mounts})
}

// Status is the instance's row of the pool's status table (no API call):
// unknown until the pool watches the instance's connection, never missing
// while the cluster cannot be asked, and unknown for no container at all. A
// ContainerID of a generation a rebuild replaced is missing, as a docker
// container a rebuild removed is (fleetd's record of the pod says which
// generation runs).
func (b *Backend) Status(containerID string) backend.LiveStatus {
	if containerID == "" {
		return backend.LiveStatusUnknown
	}
	w, err := b.where(containerID)
	if err != nil {
		return backend.LiveStatusUnknown
	}
	if b.replacedGeneration(w) {
		return backend.LiveStatusMissing
	}
	return b.p().Observe(w.cluster, w.sts).Live
}

// replacedGeneration reports whether w names an older generation of the
// instance than the one fleetd's record of its pod runs.
func (b *Backend) replacedGeneration(w where) bool {
	if b.inst.KubeCluster != w.cluster || !names.ValidHash(b.inst.KubeKey) {
		return false
	}
	rec, err := LoadInstanceRecord(w.cluster, b.inst.KubeKey)
	return err == nil && rec.StatefulSet == w.sts && w.gen < rec.Gen
}

// LastRestart is the latest restart the cluster made of the instance on its
// own (backend.RestartAware).
func (b *Backend) LastRestart(containerID string) (backend.Restart, bool) {
	w, err := b.where(containerID)
	if err != nil {
		return backend.Restart{}, false
	}
	return b.p().LastRestart(w.cluster, w.sts)
}

// PodTarget is the instance's pod as fleetd recorded it (TargetProvider):
// verified against its instance record, never against the cluster.
func (b *Backend) PodTarget() (PodTarget, error) {
	id, err := b.identity()
	if err != nil {
		return PodTarget{}, err
	}
	rec, err := LoadInstanceRecord(id.cluster, id.key)
	if err != nil {
		return PodTarget{}, err
	}
	return rec.target(), nil
}

// target is the record's pod.
func (r InstanceRecord) target() PodTarget {
	return PodTarget{
		Cluster: r.Cluster, Fleet: r.Fleet, Instance: r.Instance, StatefulSet: r.StatefulSet,
		Expect:      r.expect(),
		SpecSecret:  names.SpecSecret(r.StatefulSet, r.Gen),
		HostSockets: r.HostSockets, HostGateway: r.HostGateway, GatewaySkip: r.GatewaySkip,
	}
}

// classify gives an error from the cluster its kubernetes: code (an error
// that has one keeps it).
func classify(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := kubeerr.Of(err); ok {
		return err
	}
	if code := apiCode(err); code != "" {
		return kubeerr.Wrap(code, err)
	}
	return err
}
