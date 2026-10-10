package server

import (
	"os"
	"sort"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// interruptedFixture is one record per status, plus a creating record that
// still has a live job.
func interruptedFixture() *state.State {
	statuses := []fleet.InstanceStatus{
		fleet.StatusCreating, fleet.StatusCloning, fleet.StatusRebuilding,
		fleet.StatusRunning, fleet.StatusStopped, fleet.StatusFailed,
		fleet.StatusStarting, fleet.StatusStopping, fleet.StatusDeleting,
	}
	var insts []*fleet.Instance
	for _, s := range statuses {
		insts = append(insts, &fleet.Instance{Name: string(s), Status: s, Error: "prior"})
	}
	insts = append(insts, &fleet.Instance{Name: "live", Status: fleet.StatusCreating})
	return &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: insts}}}
}

// TestMarkInterrupted: exactly the creating, cloning and rebuilding records
// without a live job become failed with interruptedByRestart; every other
// status, and a record a job is still working on, is left alone.
func TestMarkInterrupted(t *testing.T) {
	st := interruptedFixture()
	records := markInterrupted(st, func(fleetName, instanceName string) bool {
		return fleetName == "alpha" && instanceName == "live"
	}, nil)
	var failed []string
	for _, r := range records {
		if r.err != interruptedByRestart {
			t.Errorf("%s failed with %q, want %q", r.key, r.err, interruptedByRestart)
		}
		failed = append(failed, r.key)
	}
	sort.Strings(failed)
	want := []string{"alpha/cloning", "alpha/creating", "alpha/rebuilding"}
	if len(failed) != len(want) {
		t.Fatalf("failed = %v, want %v", failed, want)
	}
	for i := range want {
		if failed[i] != want[i] {
			t.Fatalf("failed = %v, want %v", failed, want)
		}
	}
	for _, inst := range st.Fleets["alpha"].Instances {
		switch inst.Name {
		case "creating", "cloning", "rebuilding":
			if inst.Status != fleet.StatusFailed || inst.Error != interruptedByRestart {
				t.Errorf("%s = %s %q, want failed %q", inst.Name, inst.Status, inst.Error, interruptedByRestart)
			}
		case "live":
			if inst.Status != fleet.StatusCreating {
				t.Errorf("a record with a live job was reconciled: %+v", inst)
			}
		default:
			if string(inst.Status) != inst.Name || inst.Error != "prior" {
				t.Errorf("%s was touched: %+v", inst.Name, inst)
			}
		}
	}
}

// TestFailInterruptedJobsLeavesACleanStateUnwritten: a start with nothing to
// reconcile does not rewrite state.json.
func TestFailInterruptedJobsLeavesACleanStateUnwritten(t *testing.T) {
	isolateFleetDir(t)
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{{Name: "i1", Status: fleet.StatusRunning}}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(state.StatePath(), old, old); err != nil {
		t.Fatal(err)
	}
	newService().failInterruptedJobs()
	info, err := os.Stat(state.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatal("state.json rewritten although nothing was interrupted")
	}
}
