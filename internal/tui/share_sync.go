package tui

import (
	"slices"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// share_sync.go is what the TUI shows of the shared-folder sync of a fleet
// with instances on a cluster (InstanceRuntime.share_sync): on a running
// instance's row, "⟳ syncing" beside its cluster while changes are on their
// way, and "sync paused: <reason>" among its notes while a folder cannot
// sync (nothing is dropped: it waits); in the fleet options, the same pause
// lines under Custom mounts, and the host path a custom mount resolves to
// marked a synced copy, "(synced with <cluster>)". Nothing shows while all
// is in sync. Every badge is drawn in a theme slot.

// kubeSyncGlyph marks a row whose shared folders are syncing.
const kubeSyncGlyph = "⟳"

// kubeSyncBadge is what a running cluster instance's row shows beside its
// cluster while its shared folders sync ("" when in sync, or paused: the
// pause is a note).
func kubeSyncBadge(rt *fleetgrpc.InstanceRuntime) string {
	if rt.GetShareSync().GetState() != fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_SYNCING {
		return ""
	}
	return dimStyle.Render(" " + kubeSyncGlyph + " syncing")
}

// shareSyncNote is a running cluster instance's note while its shared
// folders' sync is paused ("" otherwise).
func shareSyncNote(rt *fleetgrpc.InstanceRuntime) string {
	s := rt.GetShareSync()
	if s.GetState() != fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_PAUSED {
		return ""
	}
	return "sync paused: " + s.GetReason()
}

// fleetSyncClusters are the names of the clusters a fleet has instances
// on, in order: its shared folders on the daemon host are then a copy the
// clusters' shares sync with.
func (m *model) fleetSyncClusters(fleetName string) []string {
	f := m.st.Fleets[fleetName]
	if f == nil {
		return nil
	}
	var out []string
	for _, inst := range f.Instances {
		if inst == nil || inst.Backend != fleet.BackendKubernetes || inst.KubeCluster == "" {
			continue
		}
		name := inst.KubeCluster
		if st := m.kubeStatus(inst.KubeCluster); st != nil {
			name = st.GetCluster().DisplayNameOrID()
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// fleetSyncPaused are the distinct reasons a fleet's sync is paused, from
// its instances' rows.
func (m *model) fleetSyncPaused(fleetName string) []string {
	f := m.st.Fleets[fleetName]
	if f == nil {
		return nil
	}
	var out []string
	for _, inst := range f.Instances {
		if inst == nil {
			continue
		}
		s := m.runtime[rtKey(fleetName, inst.Name)].GetShareSync()
		if s.GetState() != fleetgrpc.ShareSyncState_SHARE_SYNC_STATE_PAUSED {
			continue
		}
		for _, r := range strings.Split(s.GetReason(), "; ") {
			if r != "" && !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	return out
}

// syncedWithSuffix is the custom-mount host preview's suffix for a fleet on
// clusters: " (synced with prod-eu)" ("" for a fleet on none).
func syncedWithSuffix(clusters []string) string {
	if len(clusters) == 0 {
		return ""
	}
	return " (synced with " + strings.Join(clusters, ", ") + ")"
}
