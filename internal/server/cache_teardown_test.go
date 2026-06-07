package server

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// stubCacheTeardown swaps the deb/image cache + network teardown seams for
// recorders and restores them. Returns pointers to the "was called" flags.
func stubCacheTeardown(t *testing.T) (deb, img, net *bool) {
	t.Helper()
	var d, i, n bool
	od, oi, on := stopDebCacheServer, stopImageCacheServer, removeFleetNetwork
	stopDebCacheServer = func(string) error { d = true; return nil }
	stopImageCacheServer = func(string) error { i = true; return nil }
	removeFleetNetwork = func(string) error { n = true; return nil }
	t.Cleanup(func() {
		stopDebCacheServer, stopImageCacheServer, removeFleetNetwork = od, oi, on
	})
	return &d, &i, &n
}

// TestDestroyFleetTearsDownCachesUnconditionally guards the orphan-on-destroy
// fix: even when the cache settings are OFF at destroy time (a user disabled the
// toggle, which does NOT stop the running container), a full fleet destroy must
// still tear down the cache containers and the shared network.
func TestDestroyFleetTearsDownCachesUnconditionally(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		// Caches toggled OFF, but an instance exists.
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	deb, img, net := stubCacheTeardown(t)

	newService().destroy("alpha", "i1", true) // destroy_fleet

	if !*deb || !*img || !*net {
		t.Fatalf("destroy_fleet must tear down deb/image/network even with caches disabled: deb=%v img=%v net=%v", *deb, *img, *net)
	}
}

// TestDestroySingleInstanceLeavesCaches verifies a single-instance destroy does
// NOT tear down the shared caches/network (the fleet's other instances may use
// them).
func TestDestroySingleInstanceLeavesCaches(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{DebCacheServer: true, ImageCacheServer: true},
			Instances: []*fleet.Instance{{Name: "i1"}, {Name: "i2"}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	deb, img, net := stubCacheTeardown(t)

	newService().destroy("alpha", "i1", false) // single-instance

	if *deb || *img || *net {
		t.Fatalf("single-instance destroy must NOT tear down shared caches/network: deb=%v img=%v net=%v", *deb, *img, *net)
	}
}
