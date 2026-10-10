package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
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
