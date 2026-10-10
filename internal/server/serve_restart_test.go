package server

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// serve_restart_test.go runs the real serve loop over the ~/.fleet a previous
// daemon left behind (startServeSeeded): what a restarted daemon reconciles
// before and as it serves.

// awaitFirstBackup waits for the serve loop's immediate backup tick to write
// its archive. Serve does not wait for that goroutine, and backup paths follow
// $HOME, so a tick still running when the next test rebinds HOME would write
// (and prune temp files) in that test's ~/.fleet.
func awaitFirstBackup(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if archives, _ := filepath.Glob(filepath.Join(backupBaseDir(), "*", "*.tar.xz")); len(archives) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the serve loop never wrote its first backup")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServeFailsInterruptedRecords is the devcontainer wedge, end to end: a
// daemon that died mid-create, mid-clone and mid-rebuild left those records in
// their transitional status; the next daemon fails them before it serves, so
// the first GetState already shows them failed (and destroy or rebuild can
// act on them).
func TestServeFailsInterruptedRecords(t *testing.T) {
	client, served := startServeSeeded(t, func() {
		st := interruptedFixture()
		// No live jobs in a fresh daemon: "live" is interrupted too.
		if err := state.Save(st); err != nil {
			t.Fatalf("seed: %v", err)
		}
	})

	reply, err := client.GetState(t.Context(), &fleetgrpc.GetStateRequest{})
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	for _, inst := range reply.GetState().GetFleets()["alpha"].GetInstances() {
		switch inst.GetName() {
		case "creating", "cloning", "rebuilding", "live":
			if inst.GetStatus() != fleetgrpc.InstanceStatus_INSTANCE_STATUS_FAILED || inst.GetError() != interruptedByRestart {
				t.Errorf("%s = %v %q, want failed %q", inst.GetName(), inst.GetStatus(), inst.GetError(), interruptedByRestart)
			}
		default:
			if inst.GetStatus() == fleetgrpc.InstanceStatus_INSTANCE_STATUS_FAILED && inst.GetName() != "failed" {
				t.Errorf("%s was failed by the reconcile", inst.GetName())
			}
		}
	}
	shutdownAndWait(t, client, served)
	awaitFirstBackup(t)
}

// TestServeRestoresAutomationWatchList: the serve loop's scheduler starts from
// the watch list the previous daemon left — dropping a run whose instance is
// gone, keeping one still present, adopting an Automated instance it did not
// know — and writes the result back.
func TestServeRestoresAutomationWatchList(t *testing.T) {
	client, served := startServeSeeded(t, func() {
		// Stopped instances: the scheduler keeps watching them but leaves the
		// backend alone, so no docker is needed.
		if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "kept", Status: fleet.StatusStopped, Automated: true},
			{Name: "orphan", Status: fleet.StatusStopped, Automated: true},
		}}}}); err != nil {
			t.Fatalf("seed state: %v", err)
		}
		data, _ := json.Marshal(watchFile{Version: watchFileVersion, Runs: []watchRun{
			{Fleet: "alpha", Instance: "kept", Command: "c", Launched: true},
			{Fleet: "alpha", Instance: "gone", Command: "c", Launched: true},
		}})
		if err := writeWatchFile(state.AutomationWatchPath(), data); err != nil {
			t.Fatalf("seed watch list: %v", err)
		}
	})
	defer awaitFirstBackup(t)
	defer shutdownAndWait(t, client, served)

	deadline := time.Now().Add(10 * time.Second)
	for {
		wf, _, err := readWatchFile(state.AutomationWatchPath())
		if err == nil && len(wf.Runs) == 2 && wf.Runs[0].Instance == "kept" && !wf.Runs[0].Adopted &&
			wf.Runs[1].Instance == "orphan" && wf.Runs[1].Adopted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("watch list never reconciled: %+v (err %v)", wf, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
