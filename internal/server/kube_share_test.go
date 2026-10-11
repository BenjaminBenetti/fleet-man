//go:build !nok8s

package server

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

type recordingShares struct {
	mu   sync.Mutex
	seen []fleetShare
}

func (r *recordingShares) MaintainShare(_ context.Context, cluster, fleetName string) (fleet.ShareUsage, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, fleetShare{cluster, fleetName})
	return fleet.ShareUsage{}, true, nil
}

// TestMaintainSharesVisitsEachFleetOnEachCluster: the upkeep sees every
// fleet once per connection it has an instance on, docker instances aside.
func TestMaintainSharesVisitsEachFleetOnEachCluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"beta": {Name: "beta", Instances: []*fleet.Instance{
			{Name: "a", Backend: fleet.BackendKubernetes, KubeCluster: "c1", KubeKey: "aaaaaaaa"},
			{Name: "b", Backend: fleet.BackendKubernetes, KubeCluster: "c1", KubeKey: "bbbbbbbb"},
		}},
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "d", Backend: fleet.BackendDevcontainer},
			{Name: "k", Backend: fleet.BackendKubernetes, KubeCluster: "c2", KubeKey: "cccccccc"},
			{Name: "l", Backend: fleet.BackendKubernetes, KubeCluster: "c1", KubeKey: "dddddddd"},
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	rec := &recordingShares{}
	prev := sharePool
	sharePool = func() shareMaintainer { return rec }
	t.Cleanup(func() { sharePool = prev })
	down := map[string]bool{}
	asked := map[string]int{}
	prevUsable := shareClusterUsable
	shareClusterUsable = func(_ context.Context, cluster string) bool { asked[cluster]++; return !down[cluster] }
	t.Cleanup(func() { shareClusterUsable = prevUsable })
	maintainShares(context.Background())
	want := []fleetShare{{"c1", "alpha"}, {"c1", "beta"}, {"c2", "alpha"}}
	if !reflect.DeepEqual(rec.seen, want) {
		t.Fatalf("maintained %v, want %v", rec.seen, want)
	}

	// A connection that is not usable now is skipped, asked once a round.
	rec.seen, asked, down["c1"] = nil, map[string]int{}, true
	maintainShares(context.Background())
	if want := []fleetShare{{"c2", "alpha"}}; !reflect.DeepEqual(rec.seen, want) {
		t.Fatalf("with c1 down, maintained %v, want %v", rec.seen, want)
	}
	if asked["c1"] != 1 || asked["c2"] != 1 {
		t.Fatalf("asked whether each connection is usable %v times, want once each", asked)
	}
}
