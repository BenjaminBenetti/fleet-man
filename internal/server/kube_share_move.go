//go:build !nok8s

package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_share_move.go moves fleets' shares to their connection's shared
// StorageClass once it changed (kubernetes.Pool.MoveShare): new fleets
// take the new class at once; an existing fleet's share moves once every
// one of its instances on the connection is stopped. Until then the fleet
// is listed as waiting on the connection's status
// (KubeClusterStatus.share_migrations: Settings, `fleet cluster ls`,
// fleet_cluster_list), as moving while its copy runs, as failed (tried again
// a while later) or blocked with why.

// The states a share move is listed in.
const (
	shareMoveWaiting = "waiting"
	shareMoveMoving  = "moving"
	shareMoveFailed  = "failed"
	shareMoveBlocked = "blocked"
)

// Cadences: how often the moves are looked at besides when the instances
// change state, and how soon a failed one is tried again (env-overridable
// for tests: FLEET_KUBE_SHARE_MOVE_INTERVAL).
var (
	shareMoveEvery   = time.Minute
	shareMoveRetry   = 5 * time.Minute
	shareMoveLook    = 30 * time.Second
	shareMoveTimeout = 2 * time.Hour
)

// EnvShareMoveInterval overrides how often the moves are looked at.
const EnvShareMoveInterval = "FLEET_KUBE_SHARE_MOVE_INTERVAL"

// shareMover is the pool's side of the moves (a seam for tests).
type shareMover interface {
	ShareMoveOf(ctx context.Context, clusterID, fleetName string) (*kubernetes.ShareMove, error)
	MoveShare(ctx context.Context, clusterID, fleetName string, report func(string)) error
}

// kubeShareMoves runs the moves.
type kubeShareMoves struct {
	svc    *service
	pool   func() shareMover
	usable func(ctx context.Context, clusterID string) bool
	now    func() time.Time

	mu      sync.Mutex
	entries map[fleetShare]*shareMoveEntry
	wake    chan struct{}
	wg      sync.WaitGroup
}

// shareMoveEntry is one fleet's move as listed.
type shareMoveEntry struct {
	from, to, state, detail string
	running                 bool
	failedAt                time.Time
}

func newKubeShareMoves(svc *service) *kubeShareMoves {
	return &kubeShareMoves{
		svc:     svc,
		pool:    func() shareMover { return kubernetes.DefaultPool() },
		usable:  func(ctx context.Context, clusterID string) bool { return shareClusterUsable(ctx, clusterID) },
		now:     time.Now,
		entries: map[fleetShare]*shareMoveEntry{},
		wake:    make(chan struct{}, 1),
	}
}

// poke asks for a look soon.
func (m *kubeShareMoves) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// onState looks again when fleetd's state changes (an instance stopped).
func (m *kubeShareMoves) onState(*state.State) { m.poke() }

func shareMoveInterval() time.Duration {
	if v := os.Getenv(EnvShareMoveInterval); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		flog.Warn("kube share moves: ignoring "+EnvShareMoveInterval, "value", v)
	}
	return shareMoveEvery
}

// run looks at the moves until ctx ends, then waits for the moves it began.
func (m *kubeShareMoves) run(ctx context.Context) {
	t := time.NewTicker(shareMoveInterval())
	defer t.Stop()
	defer m.wg.Wait()
	for {
		m.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.wake:
		}
	}
}

// pass looks at every fleet's share on every connection it has an instance
// on, beginning the moves whose fleet is stopped there.
func (m *kubeShareMoves) pass(ctx context.Context) {
	st, err := state.Load()
	if err != nil {
		return
	}
	seen := map[fleetShare]bool{}
	usable := map[string]bool{}
	changed := false
	for _, fs := range fleetShares(st) {
		if ctx.Err() != nil {
			return
		}
		seen[fs] = true
		ok, known := usable[fs.cluster]
		if !known {
			ok = m.usable(ctx, fs.cluster)
			usable[fs.cluster] = ok
		}
		if !ok {
			continue
		}
		m.mu.Lock()
		e := m.entries[fs]
		running := e != nil && e.running
		m.mu.Unlock()
		if running {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, shareMoveLook)
		mv, err := m.pool().ShareMoveOf(lctx, fs.cluster, fs.fleet)
		cancel()
		if err != nil {
			flog.Info("kube share moves: looking at a share", "cluster", fs.cluster, "fleet", fs.fleet, "err", err)
			continue
		}
		changed = m.decide(ctx, st, fs, mv) || changed
	}
	m.mu.Lock()
	for fs, e := range m.entries {
		if !seen[fs] && !e.running {
			delete(m.entries, fs)
			changed = true
		}
	}
	m.mu.Unlock()
	if changed {
		m.publish()
	}
}

// decide lists fleet share fs's move mv (nil: none) and begins it when the
// fleet is stopped there. It reports whether the listing changed.
func (m *kubeShareMoves) decide(ctx context.Context, st *state.State, fs fleetShare, mv *kubernetes.ShareMove) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.entries[fs]
	if mv == nil {
		if old == nil {
			return false
		}
		delete(m.entries, fs)
		return true
	}
	e := &shareMoveEntry{from: mv.From, to: mv.To}
	if old != nil {
		e.failedAt = old.failedAt
	}
	busy := runningInstances(st, fs)
	switch {
	case mv.Untested != "":
		e.state, e.detail = shareMoveWaiting, mv.Untested
	case mv.Blocked != "":
		e.state, e.detail = shareMoveBlocked, mv.Blocked
	case busy > 0:
		e.state, e.detail = shareMoveWaiting, fmt.Sprintf("waits for %s on the cluster to stop", countPlural(busy, "running instance", "running instances"))
	case mv.NoInstance:
		e.state, e.detail = shareMoveWaiting, "moves once the fleet has an instance on the cluster, stopped (its image runs the copy)"
	case old != nil && old.state == shareMoveFailed && m.now().Sub(old.failedAt) < shareMoveRetry:
		e.state, e.detail = old.state, old.detail
	default:
		e.state, e.detail, e.running = shareMoveMoving, "copying the share", true
		m.wg.Add(1)
		// fleetd's shutdown calls off a move not past its point of no
		// return; one past it runs on (the next fleetd finishes one cut
		// short).
		go m.move(ctx, fs)
	}
	m.entries[fs] = e
	return old == nil || *old != *e
}

// move runs fleet share fs's move.
func (m *kubeShareMoves) move(ctx context.Context, fs fleetShare) {
	defer m.wg.Done()
	ctx, cancel := context.WithTimeout(ctx, shareMoveTimeout)
	defer cancel()
	err := m.pool().MoveShare(ctx, fs.cluster, fs.fleet, func(step string) {
		m.mu.Lock()
		if e := m.entries[fs]; e != nil {
			e.detail = step
		}
		m.mu.Unlock()
		m.publish()
	})
	m.mu.Lock()
	e := m.entries[fs]
	if e == nil {
		e = &shareMoveEntry{}
		m.entries[fs] = e
	}
	e.running = false
	switch {
	case err == nil, errors.Is(err, kubernetes.ErrShareRetired):
		// Moved, or ended by the fleet's destroy (its share goes with it).
		delete(m.entries, fs)
	case errors.Is(err, kubernetes.ErrShareInUse):
		// An instance started meanwhile: the move waits for the next stop.
		e.state, e.detail = shareMoveWaiting, "moves once the fleet's instances on the cluster are stopped"
	default:
		e.state, e.detail, e.failedAt = shareMoveFailed, err.Error(), m.now()
		flog.Warn("kube share moves: a fleet's share did not move; tried again later", "cluster", fs.cluster, "fleet", fs.fleet, "err", err)
	}
	m.mu.Unlock()
	m.publish()
	m.poke()
}

// runningInstances counts the instances of fs's fleet on its connection that
// are not stopped (or failed).
func runningInstances(st *state.State, fs fleetShare) int {
	f := st.Fleets[fs.fleet]
	if f == nil {
		return 0
	}
	n := 0
	for _, inst := range f.Instances {
		if inst == nil || inst.Backend != fleet.BackendKubernetes || inst.KubeCluster != fs.cluster {
			continue
		}
		if inst.Status != fleet.StatusStopped && inst.Status != fleet.StatusFailed {
			n++
		}
	}
	return n
}

func countPlural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// status is connection clusterID's moves, by fleet.
func (m *kubeShareMoves) status(clusterID string) []*fleetgrpc.KubeShareMigration {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*fleetgrpc.KubeShareMigration
	for fs, e := range m.entries {
		if fs.cluster == clusterID && e.state != "" {
			out = append(out, &fleetgrpc.KubeShareMigration{Fleet: fs.fleet, FromClass: e.from, ToClass: e.to, State: e.state, Detail: e.detail})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetFleet() < out[j].GetFleet() })
	return out
}

// publish pushes the connections' statuses (the moves are on them).
func (m *kubeShareMoves) publish() {
	if m.svc != nil && m.svc.kube != nil {
		m.svc.kube.publish()
	}
}
