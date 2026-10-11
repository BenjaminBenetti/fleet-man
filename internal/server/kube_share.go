//go:build !nok8s

package server

import (
	"context"
	"sort"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// kube_share.go is the upkeep of the fleets' shares on their clusters (the
// volume a fleet's agent dirs, shared files and custom mounts live on there):
// with the shared services' upkeep, each fleet's share on each connection it
// has an instance on grows when nearly full where it can, else warns
// (kubernetes.Pool.MaintainShare).

// shareUpkeepTimeout bounds one share's upkeep.
const shareUpkeepTimeout = 2 * time.Minute

// sharePool is the pool's share upkeep (a seam for tests).
var sharePool = func() shareMaintainer { return kubernetes.DefaultPool() }

type shareMaintainer interface {
	MaintainShare(ctx context.Context, clusterID, fleetName string) (fleet.ShareUsage, bool, error)
}

// maintainShares runs the upkeep of every fleet's share on every connection
// it has an instance on, one at a time; best-effort, logged.
func maintainShares(ctx context.Context) {
	st, err := state.Load()
	if err != nil {
		return
	}
	for _, s := range fleetShares(st) {
		if ctx.Err() != nil {
			return
		}
		sctx, cancel := context.WithTimeout(ctx, shareUpkeepTimeout)
		if _, _, err := sharePool().MaintainShare(sctx, s.cluster, s.fleet); err != nil && ctx.Err() == nil {
			flog.Warn("kube share upkeep failed", "cluster", s.cluster, "fleet", s.fleet, "err", err)
		}
		cancel()
	}
}

// fleetShare is a fleet on a connection.
type fleetShare struct{ cluster, fleet string }

// fleetShares are the (connection, fleet) pairs st has instances on, in
// order.
func fleetShares(st *state.State) []fleetShare {
	seen := map[fleetShare]bool{}
	var out []fleetShare
	for name, f := range st.Fleets {
		if f == nil {
			continue
		}
		for _, inst := range f.Instances {
			if inst == nil || inst.Backend != fleet.BackendKubernetes || inst.KubeCluster == "" {
				continue
			}
			s := fleetShare{inst.KubeCluster, name}
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].cluster != out[j].cluster {
			return out[i].cluster < out[j].cluster
		}
		return out[i].fleet < out[j].fleet
	})
	return out
}
