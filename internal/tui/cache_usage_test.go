package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// TestEditFleetCachingShowsClusterUsage: expanding Caching asks the daemon
// how full the fleet's cluster cache volumes are, and each enabled cache's
// row shows its volume on each cluster, its server's state when it is not
// ready, and the 90% warning; a report of another fleet is dropped.
func TestEditFleetCachingShowsClusterUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubFleetSettingsSave(t)
	var asked []string
	orig := getCacheUsageRemote
	getCacheUsageRemote = func(fleetName string) (*fleetgrpc.GetCacheUsageReply, error) {
		asked = append(asked, fleetName)
		return &fleetgrpc.GetCacheUsageReply{Caches: []*fleetgrpc.CacheUsage{
			{Service: "debcache", Location: "minikube", UsedBytes: 95 * (1 << 30) / 10, CapacityBytes: 10 << 30, State: "ready", Warning: "95% full; its StorageClass cannot grow it"},
			{Service: "imagecache", Location: "minikube", UsedBytes: 0, CapacityBytes: 20 << 30, State: "starting"},
		}}, nil
	}
	t.Cleanup(func() { getCacheUsageRemote = orig })

	f := &fleet.Fleet{Name: "alpha", Settings: fleet.FleetSettings{DebCacheServer: true, ImageCacheServer: true}}
	fp := newFleetPage()
	fp.rows = []row{{kind: rowFleetHeader, fleetName: "alpha"}}
	m := &model{st: &state.State{Fleets: map[string]*fleet.Fleet{"alpha": f}}, fleetPage: fp}
	fp.updateNormal(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	for guard := 0; fp.dlg.row != editFleetRowCaching; guard++ {
		fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyDown})
		if guard > 20 {
			t.Fatal("never reached the Caching header")
		}
	}
	cmd := fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if cmd == nil {
		t.Fatal("expanding Caching asked for no usage")
	}
	msg, ok := cmd().(cacheUsageMsg)
	if !ok || len(asked) != 1 || asked[0] != "alpha" {
		t.Fatalf("usage asked for %v (%T)", asked, msg)
	}
	fp.handleCacheUsage(msg)

	view := fp.renderEditFleet(m)
	for _, want := range []string{
		"☸ minikube 9.5 GiB of 10.0 GiB (95%)",
		"⚠ 95% full; its StorageClass cannot grow it",
		"☸ minikube 0 B of 20.0 GiB (0%) · starting",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	// Collapsing and expanding again asks again; a report of another fleet
	// (the dialog moved on) is dropped.
	fp.handleCacheUsage(cacheUsageMsg{fleet: "beta", reply: &fleetgrpc.GetCacheUsageReply{}})
	if len(fp.editFleet.cacheUsage) != 2 {
		t.Fatal("another fleet's report replaced this one's")
	}
	fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	if cmd := fp.updateEditFleet(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}); cmd == nil {
		t.Fatal("expanding Caching again asked for no usage")
	}
}

func TestFormatCacheBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 10 << 30: "10.0 GiB", 3 << 40: "3.0 TiB"} {
		if got := formatCacheBytes(n); got != want {
			t.Errorf("%d = %q, want %q", n, got, want)
		}
	}
}

// A cluster cache wipe scales the server down, remakes its volume and waits
// for the new pod: the client waits longer than the daemon's 270s for it.
func TestDeleteCacheTimeoutCoversAClusterWipe(t *testing.T) {
	if deleteCacheTimeout != 300*time.Second {
		t.Fatalf("deleteCacheTimeout = %v", deleteCacheTimeout)
	}
}
