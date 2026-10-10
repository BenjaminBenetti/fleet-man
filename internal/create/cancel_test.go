package create

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// downRecorder is a backend whose only working method is Down, which records
// the container ids it was asked to tear down. Anything else panics, proving
// the code under test touched nothing but Down.
type downRecorder struct {
	backend.Backend
	downs []string
}

func (d *downRecorder) Down(containerID string) error {
	d.downs = append(d.downs, containerID)
	return nil
}

var errTestDestroyed = errors.New("instance destroyed")

// cancelledCtx is a job context cancelled the way a destroy cancels one.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errTestDestroyed)
	return ctx
}

// seedInstance writes a one-instance fleet to a fresh temp HOME.
func seedInstance(t *testing.T, inst *fleet.Instance) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Remote: "git@example.com:a.git", Instances: []*fleet.Instance{inst}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func loadInstance(t *testing.T, name string) *fleet.Instance {
	t.Helper()
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	f, ok := st.Fleets["alpha"]
	if !ok {
		return nil
	}
	inst, err := f.GetInstance(name)
	if err != nil {
		return nil
	}
	return inst
}

// TestCommitRunning covers the commit a provisioning run ends with: a live run
// records its container, while a run whose job was cancelled, or whose record
// was destroyed or marked deleting, tears the container down and commits
// nothing — the destroy-cancels-create half that lives in the run itself.
func TestCommitRunning(t *testing.T) {
	cases := []struct {
		name        string
		status      fleet.InstanceStatus
		remove      bool // the record is gone (a destroy already removed it)
		cancel      bool
		wantErr     error
		wantStatus  fleet.InstanceStatus
		wantRunning bool
	}{
		{name: "live run commits", status: fleet.StatusCreating, wantStatus: fleet.StatusRunning, wantRunning: true},
		{name: "cancelled run fails", status: fleet.StatusCreating, cancel: true, wantErr: errCancelled, wantStatus: fleet.StatusFailed},
		{name: "deleting record is left to the destroy", status: fleet.StatusDeleting, wantErr: errAbandoned, wantStatus: fleet.StatusDeleting},
		{name: "cancelled and deleting", status: fleet.StatusDeleting, cancel: true, wantErr: errCancelled, wantStatus: fleet.StatusDeleting},
		{name: "removed record", remove: true, wantErr: errAbandoned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.remove {
				seedInstance(t, &fleet.Instance{Name: "other", Status: fleet.StatusRunning})
			} else {
				seedInstance(t, &fleet.Instance{Name: "i1", Status: tc.status})
			}
			ctx := context.Background()
			if tc.cancel {
				ctx = cancelledCtx()
			}
			b := &downRecorder{}

			err := commitRunning(ctx, b, "alpha", "i1", "cid-1")

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("commitRunning: %v", err)
				}
				if len(b.downs) != 0 {
					t.Fatalf("a committed container must not be torn down: %v", b.downs)
				}
			} else {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if len(b.downs) != 1 || b.downs[0] != "cid-1" {
					t.Fatalf("an abandoned run must tear its container down, downs = %v", b.downs)
				}
			}
			inst := loadInstance(t, "i1")
			if tc.remove {
				if inst != nil {
					t.Fatalf("a removed record came back: %+v", inst)
				}
				return
			}
			if inst.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s", inst.Status, tc.wantStatus)
			}
			if tc.wantRunning != (inst.ContainerID == "cid-1") {
				t.Fatalf("container id recorded = %q, want recorded=%v", inst.ContainerID, tc.wantRunning)
			}
		})
	}
}

// TestCancelledRunNamesTheCause: the failure a cancelled run records reads as
// the reason its job was cancelled.
func TestCancelledRunNamesTheCause(t *testing.T) {
	seedInstance(t, &fleet.Instance{Name: "i1", Status: fleet.StatusCreating})
	err := commitRunning(cancelledCtx(), &downRecorder{}, "alpha", "i1", "cid-1")
	if err == nil || err.Error() != "cancelled: instance destroyed" {
		t.Fatalf("err = %v, want %q", err, "cancelled: instance destroyed")
	}
	if inst := loadInstance(t, "i1"); inst.Error != err.Error() {
		t.Fatalf("recorded error = %q, want %q", inst.Error, err.Error())
	}
}

func TestDiscardIfCancelled(t *testing.T) {
	b := &downRecorder{}
	if err := discardIfCancelled(context.Background(), b, "alpha", "i1", "cid-1"); err != nil || len(b.downs) != 0 {
		t.Fatalf("a live run must keep its container: err=%v downs=%v", err, b.downs)
	}
	if err := discardIfCancelled(cancelledCtx(), b, "alpha", "i1", "cid-1"); !errors.Is(err, errCancelled) || !errors.Is(err, errTestDestroyed) {
		t.Fatalf("err = %v, want the cancellation and its cause", err)
	}
	if len(b.downs) != 1 || b.downs[0] != "cid-1" {
		t.Fatalf("a cancelled run must tear its container down, downs = %v", b.downs)
	}
	// No container yet: nothing to tear down.
	b.downs = nil
	if err := discardIfCancelled(cancelledCtx(), b, "alpha", "i1", ""); err == nil || len(b.downs) != 0 {
		t.Fatalf("err=%v downs=%v, want an error and no teardown", err, b.downs)
	}
}

func TestSetFailedLeavesDeletingRecords(t *testing.T) {
	seedInstance(t, &fleet.Instance{Name: "i1", Status: fleet.StatusDeleting})
	setFailed("alpha", "i1", errors.New("boom"))
	if inst := loadInstance(t, "i1"); inst.Status != fleet.StatusDeleting || inst.Error != "" {
		t.Fatalf("a record being destroyed must not flip to failed: %+v", inst)
	}

	seedInstance(t, &fleet.Instance{Name: "i1", Status: fleet.StatusCreating})
	setFailed("alpha", "i1", errors.New("boom"))
	if inst := loadInstance(t, "i1"); inst.Status != fleet.StatusFailed || inst.Error != "boom" {
		t.Fatalf("setFailed = %+v, want failed with the error", inst)
	}
}

// TestRunStopsWhenCancelledBeforeWork: a create whose job was cancelled before
// it got going (destroyed while queued behind the host-key prompt, say) does no
// work at all — no clone — and fails with the reason.
func TestRunStopsWhenCancelledBeforeWork(t *testing.T) {
	seedInstance(t, &fleet.Instance{Name: "i1", Status: fleet.StatusCreating})
	// An unreachable remote: a clone attempt would fail with a git error.
	err := RunContext(cancelledCtx(), "alpha", "i1", "/nonexistent/repo.git", "", false, fleet.BackendDevcontainer)
	if !errors.Is(err, errCancelled) {
		t.Fatalf("err = %v, want the cancellation (no clone attempted)", err)
	}
	if inst := loadInstance(t, "i1"); inst.Status != fleet.StatusFailed || !strings.Contains(inst.Error, "instance destroyed") {
		t.Fatalf("record = %+v, want failed with the cancellation cause", inst)
	}
}

// TestRunCloneStopsWhenCancelledBeforeCopy: a cancelled clone stops before it
// copies the source workspace or commits the source container.
func TestRunCloneStopsWhenCancelledBeforeCopy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := t.TempDir()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: []*fleet.Instance{
			{Name: "src", Status: fleet.StatusRunning, ContainerID: "src-cid", WorkspaceDir: src, Backend: fleet.BackendDevcontainer},
			{Name: "dst", Status: fleet.StatusCloning, Backend: fleet.BackendDevcontainer},
		}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := RunClone(cancelledCtx(), "alpha", "src", "dst", false)
	if !errors.Is(err, errCancelled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if inst := loadInstance(t, "dst"); inst.Status != fleet.StatusFailed {
		t.Fatalf("dest record = %+v, want failed", inst)
	}
}
