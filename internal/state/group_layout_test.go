package state

import (
	"reflect"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

func TestLoadMigratesGroupLayouts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	current := GroupLayout{FleetName: "one", InstanceName: "unique", GroupID: "current", Layout: "new geometry"}
	legacy := GroupLayout{InstanceName: "unique", GroupID: "legacy", Sessions: []string{"unique~legacy"}, Layout: "old geometry", PaneCount: 1}
	st := &State{
		Fleets: map[string]*fleet.Fleet{
			"one": {Instances: []*fleet.Instance{{Name: "unique"}, {Name: "shared"}}},
			"two": {Instances: []*fleet.Instance{{Name: "shared"}}},
		},
		GroupLayouts: map[string]GroupLayout{
			"unique/legacy":    legacy,
			"shared/ambiguous": {InstanceName: "shared", GroupID: "ambiguous"},
			"missing/orphan":   {InstanceName: "missing", GroupID: "orphan"},
			"unique/current":   {InstanceName: "unique", GroupID: "current", Layout: "stale geometry"},
			current.Key():      current,
		},
	}
	if err := Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	legacy.FleetName = "one"
	want := map[string]GroupLayout{legacy.Key(): legacy, current.Key(): current}
	if !reflect.DeepEqual(got.GroupLayouts, want) {
		t.Fatalf("layouts = %#v, want %#v", got.GroupLayouts, want)
	}
	if err := Save(got); err != nil {
		t.Fatal(err)
	}
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.GroupLayouts, want) {
		t.Fatalf("migration did not survive reload: %#v", again.GroupLayouts)
	}
}
