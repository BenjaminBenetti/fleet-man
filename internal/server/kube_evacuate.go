//go:build !nok8s

package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/instanceops"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/topology"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_evacuate.go is fleetd's side of moving a fleet off a node the cluster
// empties (kubernetes.Pool.Evacuate). kubePods sees it on any running
// instance whose pod holds its fleet's share in single-node mode on such a
// node, or whose StatefulSet still carries the mark of a move a previous
// daemon began, and asks for a move of the fleet; the move runs once per
// fleet and connection at a time, over every instance of the fleet there
// that fleetd's state says runs, each stopped and started again as a Fleet
// stop and start do. The records stay running throughout (no one stopped
// them): the rows say "node cordoned: fleet moving", and once the pods run
// on another node "sessions reset: fleet moved". An instance that does not
// start again is recorded stopped, the reason in its warning and error, as a
// failed Fleet start leaves it. A fleet that cannot move (its volumes are on
// the node's own disk) is left as it is, its rows and the connection's
// status saying why, until the node is no longer being emptied.
//
// The connection's status (Settings, `fleet cluster ls`) carries the moves
// in progress and the fleets that cannot move as warnings, and
// KubeCapabilities.node_cordoned_moving while one is in progress.

// evacKey is one fleet on one connection.
type evacKey struct{ cluster, fleet string }

// evacMove is a move in progress.
type evacMove struct {
	node, why string
	since     time.Time
}

// evacHold keeps a fleet from being asked to move off node again: until
// its holders are on a node not being emptied (until zero: a fleet that
// cannot move, blocked saying why, or one with nothing to move), or until
// until (a move that ended, or could not begin, is not asked again at once).
type evacHold struct {
	node, blocked string
	until         time.Time
}

// evacRetry is how soon a move that ended, or could not begin, may be asked
// again (while the fleet is still on the node).
const evacRetry = time.Minute

// evacUnmarkTimeout bounds the removal of a stale moving mark.
const evacUnmarkTimeout = 30 * time.Second

// kubeEvacuations runs kubePods' moves.
type kubeEvacuations struct {
	k *kubePods
	// evacuate is the pool's Evacuate, members the move's instances from
	// fleetd's records (kubePods.evacMembers); tests stand in their own.
	evacuate func(ctx context.Context, e kubernetes.Evacuation) (kubernetes.EvacuationResult, error)
	members  func(cluster, fleetName string) (share string, ms []evacMember)
	// clearMark is the pool's ClearMovingOff (tests stand in their own).
	clearMark func(ctx context.Context, cluster, sts string) error
	// flushBeforeStop is the share sync's flush before a Fleet stop
	// (shareSync.beforeStop), run by each member before its pod stops.
	flushBeforeStop func(fleetName, instance string)

	mu     sync.Mutex
	moving map[evacKey]evacMove
	held   map[evacKey]evacHold
	// unmarking are the instances whose stale mark is being removed, with
	// their fleets: a move of one waits for it (the removal must not land on
	// the move's own mark).
	unmarking map[kubeRef]evacKey
	// deferred are the fleets whose move waited for a removal: looked at
	// again once it is done. unmarkAfter is when a removal that failed may
	// be tried again.
	deferred    map[evacKey]bool
	unmarkAfter map[kubeRef]time.Time
	// retry is evacRetry (tests shorten it); done is told when a move ends
	// (tests).
	retry time.Duration
	done  func(evacKey, kubernetes.EvacuationResult)
}

// evacMember is one instance a move takes along.
type evacMember struct {
	name, sts, key string
	stop, start    func() error
}

func newKubeEvacuations(k *kubePods) *kubeEvacuations {
	e := &kubeEvacuations{
		k: k,
		evacuate: func(ctx context.Context, ev kubernetes.Evacuation) (kubernetes.EvacuationResult, error) {
			return kubernetes.DefaultPool().Evacuate(ctx, ev)
		},
		clearMark: func(ctx context.Context, cluster, sts string) error {
			return kubernetes.DefaultPool().ClearMovingOff(ctx, cluster, sts)
		},
		// Looked up when called: the service makes its share sync after
		// kubePods.
		flushBeforeStop: func(fleetName, instance string) {
			if svc := k.svc; svc != nil && svc.shareSync != nil {
				svc.shareSync.beforeStop(fleetName, instance)
			}
		},
		moving:      map[evacKey]evacMove{},
		held:        map[evacKey]evacHold{},
		unmarking:   map[kubeRef]evacKey{},
		deferred:    map[evacKey]bool{},
		unmarkAfter: map[kubeRef]time.Time{},
		retry:       evacRetry,
	}
	e.members = e.recordMembers
	return e
}

// want asks for a move of the fleet off node, unless one runs, or the fleet
// is held on that node (it cannot move, or a move just ended).
func (e *kubeEvacuations) want(ctx context.Context, cluster, fleetName, node, why string) {
	key := evacKey{cluster, fleetName}
	now := e.k.now()
	e.mu.Lock()
	if _, busy := e.moving[key]; busy {
		e.mu.Unlock()
		return
	}
	if h, ok := e.held[key]; ok && h.node == node && (h.until.IsZero() || now.Before(h.until)) {
		e.mu.Unlock()
		return
	}
	for _, k := range e.unmarking {
		if k == key {
			// Asked again once the removal is done.
			e.deferred[key] = true
			e.mu.Unlock()
			return
		}
	}
	delete(e.held, key)
	e.moving[key] = evacMove{node: node, why: why, since: now}
	e.mu.Unlock()
	e.changed(key)
	go e.run(ctx, key, node, why)
}

// settled is told that a holder of the fleet's share is on a node not being
// emptied: a fleet held because it could not move (or had nothing to move)
// may be asked again.
func (e *kubeEvacuations) settled(cluster, fleetName string) {
	key := evacKey{cluster, fleetName}
	e.mu.Lock()
	h, ok := e.held[key]
	ok = ok && h.until.IsZero()
	if ok {
		delete(e.held, key)
	}
	e.mu.Unlock()
	if ok && h.blocked != "" {
		e.changed(key)
	}
}

// unmark removes the moving mark an instance of fleetName kept after its
// move was over (it runs again, or was stopped, and no move of its fleet is
// in progress), in the background; a failure is tried again at an
// evaluation after the retry pause.
func (e *kubeEvacuations) unmark(ctx context.Context, ref kubeRef, fleetName string) {
	key := evacKey{ref.cluster, fleetName}
	e.mu.Lock()
	if _, busy := e.unmarking[ref]; busy {
		e.mu.Unlock()
		return
	}
	if _, busy := e.moving[key]; busy {
		e.mu.Unlock()
		return
	}
	if after, ok := e.unmarkAfter[ref]; ok && e.k.now().Before(after) {
		e.mu.Unlock()
		return
	}
	e.unmarking[ref] = key
	e.mu.Unlock()
	go func() {
		uctx, cancel := context.WithTimeout(ctx, evacUnmarkTimeout)
		err := e.clearMark(uctx, ref.cluster, ref.sts)
		cancel()
		e.mu.Lock()
		delete(e.unmarking, ref)
		if err != nil {
			e.unmarkAfter[ref] = e.k.now().Add(e.retry)
		} else {
			delete(e.unmarkAfter, ref)
		}
		again := e.deferred[key]
		for _, k := range e.unmarking {
			again = again && k != key
		}
		if again {
			delete(e.deferred, key)
		}
		e.mu.Unlock()
		if again {
			// A move asked for meanwhile waited for this.
			e.k.pokeFleet(key.cluster, key.fleet)
		}
		if err != nil && ctx.Err() == nil {
			flog.Warn("kube evacuate: removing the mark of a move that is over", "cluster", ref.cluster, "sts", ref.sts, "err", err)
			return
		}
		flog.Info("kube evacuate: removed the mark of a move that is over", "cluster", ref.cluster, "sts", ref.sts)
	}()
}

// prune forgets the holds of the fleets of which no instance runs on the
// connection any more (all stopped, or destroyed): none is on the node, and
// the connection's status must not say one stays there.
func (e *kubeEvacuations) prune(running map[evacKey]bool) {
	var gone []evacKey
	e.mu.Lock()
	for key, h := range e.held {
		if !running[key] {
			delete(e.held, key)
			if h.blocked != "" {
				gone = append(gone, key)
			}
		}
	}
	e.mu.Unlock()
	for _, key := range gone {
		e.changed(key)
	}
}

// state is what the rows of the fleet's instances on the connection say:
// moving, or why the fleet cannot move ("").
func (e *kubeEvacuations) state(cluster, fleetName string) (moving bool, blocked string) {
	key := evacKey{cluster, fleetName}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, moving = e.moving[key]
	return moving, e.held[key].blocked
}

// status is what a connection's status says of the moves on it: whether one
// is in progress, and a warning per fleet moving or unable to move.
func (e *kubeEvacuations) status(cluster string) (moving bool, warnings []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, m := range e.moving {
		if key.cluster == cluster {
			moving = true
			warnings = append(warnings, fmt.Sprintf("node cordoned: fleet moving (%s off %s, %s)", key.fleet, m.node, m.why))
		}
	}
	for key, h := range e.held {
		if key.cluster == cluster && h.blocked != "" {
			warnings = append(warnings, fmt.Sprintf("node cordoned: fleet can't move (%s stays on %s: %s)", key.fleet, h.node, h.blocked))
		}
	}
	sort.Strings(warnings)
	return moving, warnings
}

// changed re-evaluates the fleet's instances (their rows) and pushes the
// connection's status.
func (e *kubeEvacuations) changed(key evacKey) {
	e.k.pokeFleet(key.cluster, key.fleet)
	if svc := e.k.svc; svc != nil && svc.kube != nil {
		svc.kube.publish()
	}
}

// run is one move.
func (e *kubeEvacuations) run(ctx context.Context, key evacKey, node, why string) {
	var res kubernetes.EvacuationResult
	// Held afterwards: until the fleet is on a node not being emptied when
	// it cannot move (or there was nothing to move), else for a while.
	hold := evacHold{node: node, until: e.k.now().Add(e.retry)}
	defer func() {
		if res.Blocked != "" {
			hold.blocked, hold.until = res.Blocked, time.Time{}
		}
		e.mu.Lock()
		delete(e.moving, key)
		e.held[key] = hold
		done := e.done
		e.mu.Unlock()
		e.changed(key)
		if done != nil {
			done(key, res)
		}
	}()
	share, ms := e.members(key.cluster, key.fleet)
	if len(ms) == 0 {
		hold.until = time.Time{}
		return
	}
	ev := kubernetes.Evacuation{Cluster: key.cluster, Node: node, Why: why, Fleet: key.fleet, Share: share}
	for _, m := range ms {
		key, fleetName, name := m.key, key.fleet, m.name
		ev.Members = append(ev.Members, kubernetes.EvacuationMember{
			Instance: m.name, StatefulSet: m.sts, Stop: m.stop, Start: m.start,
			Wanted: func() bool { return recordRunning(fleetName, name, key) },
		})
	}
	var err error
	res, err = e.evacuate(ctx, ev)
	switch {
	case ctx.Err() != nil:
		// fleetd is stopping: whatever did not start again is the next
		// daemon's to finish (the marks stay), not a failed start.
		return
	case err != nil:
		flog.Warn("kube evacuate: the move could not begin", "cluster", key.cluster, "fleet", key.fleet, "node", node, "err", err)
		return
	}
	failed := false
	for i, o := range res.Outcomes {
		if o.Err == nil {
			continue
		}
		m := ms[i]
		msg := fmt.Sprintf("%v (moving the fleet off node %s, which the cluster is emptying)", o.Err, node)
		if !o.Stopped {
			state.WriteWarn(key.fleet, m.name, msg)
			continue
		}
		// Stopped and not started again: what a failed Fleet start leaves.
		if recordStopped(key.fleet, m.name, m.key, msg) {
			state.WriteWarn(key.fleet, m.name, msg)
			failed = true
		}
	}
	if failed && e.k.svc != nil {
		loadAndPush(e.k.svc.hub)
	}
}

// recordMembers are the fleet's instances on the connection that fleetd's
// state says run and whose record says their share is in single-node mode,
// each stopped and started through its backend handle, the stop preceded by
// the share sync's flush as a Fleet stop's is (the start carries the Ready
// gate itself), the start followed by fleet's own post-start work as after a
// Fleet start; and the fleet's share PVC.
func (e *kubeEvacuations) recordMembers(cluster, fleetName string) (string, []evacMember) {
	st, err := state.Load()
	if err != nil {
		flog.Warn("kube evacuate: load state", "err", err)
		return "", nil
	}
	f := st.Fleets[fleetName]
	if f == nil {
		return "", nil
	}
	share := ""
	var out []evacMember
	for _, inst := range f.Instances {
		if !isKubeInstance(inst) || inst.KubeCluster != cluster || inst.Status != fleet.StatusRunning {
			continue
		}
		rec, err := kubernetes.LoadInstanceRecord(cluster, inst.KubeKey)
		if err != nil || rec.Share == nil || topology.Mode(rec.Share.Mode) != topology.SingleNode {
			continue
		}
		share = rec.Share.Claim
		i := *inst
		home := f.Settings.HomeDir
		out = append(out, evacMember{
			name: i.Name, sts: rec.StatefulSet, key: i.KubeKey,
			stop: func() error {
				// The pod's last changes to the fleet's shared folders reach
				// the daemon host first (bounded; a share that could not
				// flush is remembered, and a destroy keeps it).
				e.flushBeforeStop(fleetName, i.Name)
				return backendutil.NewForInstance(&i, false).Stop(i.ContainerID)
			},
			start: func() error {
				if err := backendutil.NewForInstance(&i, false).Start(i.ContainerID); err != nil {
					return err
				}
				instanceops.ReviveSharedServices(fleetName, &i)
				instanceops.RunPostStartHook(fleetName, &i, home)
				e.k.started(fleetName, i.Name)
				return nil
			},
		})
	}
	return share, out
}

// recordRunning reports whether fleetName/name is still the running instance
// with key.
func recordRunning(fleetName, name, key string) bool {
	st, err := state.Load()
	if err != nil {
		return true
	}
	f := st.Fleets[fleetName]
	if f == nil {
		return false
	}
	inst, err := f.GetInstance(name)
	return err == nil && inst.KubeKey == key && inst.Status == fleet.StatusRunning
}

// recordStopped records fleetName/name stopped with msg as its error, if it
// is still the running instance with key.
func recordStopped(fleetName, name, key, msg string) bool {
	changed := false
	err := state.Update(func(st *state.State) error {
		f := st.Fleets[fleetName]
		if f == nil {
			return nil
		}
		inst, err := f.GetInstance(name)
		if err != nil || inst.KubeKey != key || inst.Status != fleet.StatusRunning {
			return nil
		}
		inst.Status = fleet.StatusStopped
		inst.Error = strings.TrimSpace(msg)
		changed = true
		return nil
	})
	if err != nil {
		flog.Error("kube evacuate: recording an instance that did not start again", "fleet", fleetName, "instance", name, "err", err)
		return false
	}
	return changed
}
