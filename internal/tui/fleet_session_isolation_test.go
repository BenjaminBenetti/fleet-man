package tui

import (
	"reflect"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

func TestSavedSessionsIsolatedAcrossFleets(t *testing.T) {
	previousSet, previousDelete := setGroupLayoutRemote, deleteGroupLayoutRemote
	setGroupLayoutRemote = func(configutil.GroupLayout) error { return nil }
	var deleted []string
	deleteGroupLayoutRemote = func(f, i, g string) error { deleted = append(deleted, computeGroupKey(f, i, g)); return nil }
	t.Cleanup(func() { setGroupLayoutRemote, deleteGroupLayoutRemote = previousSet, previousDelete })

	m := &model{
		st:                 &configutil.State{Fleets: map[string]*fleet.Fleet{}},
		fleetPage:          newFleetPage(),
		sessionStore:       NewSessionStore(),
		recentGroupLayouts: map[string]time.Time{},
	}
	for _, name := range []string{"one", "two"} {
		m.st.Fleets[name] = &fleet.Fleet{Name: name, Instances: []*fleet.Instance{{Name: "alpha", Status: fleet.StatusRunning}}}
		ref := InstanceRef{Fleet: name, Instance: "alpha"}
		m.sessionStore.SetExpanded(ref, true)
		m.recordPresetGroupLayout(presetSessionsCreatedMsg{ref: ref, groupID: "shared", sessions: []string{"alpha~shared"}, layout: name})
	}
	if len(m.st.GroupLayouts) != 2 {
		t.Fatalf("layouts collided: %#v", m.st.GroupLayouts)
	}
	// Renaming one fleet's saved group must not rename the other fleet's group.
	ref := InstanceRef{Fleet: "one", Instance: "alpha"}
	m.migrateRenamedSession(sessionRenamedMsg{ref: ref, oldGroupID: "shared", newGroupID: "renamed"})
	m.fleetPage.buildRows(m)
	got := map[string][]string{}
	for _, row := range m.fleetPage.rows {
		if row.kind == rowSession {
			got[row.fleetName] = append(got[row.fleetName], row.groupID)
		}
	}
	want := map[string][]string{"one": {"renamed"}, "two": {"shared"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("session rows crossed fleets: %v, want %v", got, want)
	}
	// A discovery for one/alpha cannot prune two/alpha's saved layout.
	m.recentGroupLayouts = nil
	m.sessionStore.SetDiscovery(ref, []tmuxSession{{Name: "alpha~live"}})
	m.pruneSavedGroupsForInstance(ref)
	if _, ok := m.st.GroupLayouts["two/alpha/shared"]; !ok {
		t.Fatal("pruned the other fleet's layout")
	}
	// Removing one fleet does not leave its saved groups attached to the other.
	delete(m.st.Fleets, "two")
	m.pruneOrphanedSavedGroups()
	if len(m.st.GroupLayouts) != 0 {
		t.Fatalf("orphaned layouts remain: %v", m.st.GroupLayouts)
	}
	if !reflect.DeepEqual(deleted, []string{"one/alpha/shared", "one/alpha/renamed", "two/alpha/shared"}) {
		t.Fatalf("wrong layout deletions: %v", deleted)
	}
}
