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
// records its container; a run whose record was destroyed or marked deleting
// tears the container down and commits nothing (the destroy-cancels-create
// half that lives in the run itself); a run the daemon's shutdown cancelled
// keeps its container on the failed record.
func TestCommitRunning(t *testing.T) {
	cases := []struct {
		name          string
		status        fleet.InstanceStatus
		remove        bool // the record is gone (a destroy already removed it)
		cancel        bool
		wantErr       error
		wantStatus    fleet.InstanceStatus
		wantContainer bool // the record holds cid-1
		wantDiscard   bool // cid-1 was torn down
	}{
		{name: "live run commits", status: fleet.StatusCreating, wantStatus: fleet.StatusRunning, wantContainer: true},
		{name: "shutdown keeps the container on the failed record", status: fleet.StatusCreating, cancel: true, wantErr: errCancelled, wantStatus: fleet.StatusFailed, wantContainer: true},
		{name: "deleting record is left to the destroy", status: fleet.StatusDeleting, wantErr: errAbandoned, wantStatus: fleet.StatusDeleting, wantDiscard: true},
		{name: "cancelled and deleting", status: fleet.StatusDeleting, cancel: true, wantErr: errCancelled, wantStatus: fleet.StatusDeleting, wantDiscard: true},
		{name: "removed record", remove: true, wantErr: errAbandoned, wantDiscard: true},
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

			if tc.wantErr == nil && err != nil {
				t.Fatalf("commitRunning: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if discarded := len(b.downs) == 1 && b.downs[0] == "cid-1"; discarded != tc.wantDiscard || len(b.downs) > 1 {
				t.Fatalf("downs = %v, want discard=%v", b.downs, tc.wantDiscard)
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
			if tc.wantContainer != (inst.ContainerID == "cid-1") {
				t.Fatalf("container id recorded = %q, want recorded=%v", inst.ContainerID, tc.wantContainer)
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

// TestStopIfCancelled: after a step produced a container, a live run goes on;
// a cancelled run stops there, tearing the container down only when a destroy
// has its record. A shutdown leaves the record failed and holding the
// container, the one a rebuild just made included: for codespaces that is the
// codespace itself, which a teardown would delete with the user's work.
func TestStopIfCancelled(t *testing.T) {
	cases := []struct {
		name          string
		status        fleet.InstanceStatus
		prior         string // the record's ContainerID before the step
		ctx           context.Context
		containerID   string
		wantErr       bool
		wantDiscard   bool
		wantStatus    fleet.InstanceStatus
		wantContainer string
	}{
		{name: "live run goes on", status: fleet.StatusCreating, ctx: context.Background(), containerID: "cid-1", wantStatus: fleet.StatusCreating},
		{name: "destroy tears the container down", status: fleet.StatusDeleting, ctx: cancelledCtx(), containerID: "cid-1", wantErr: true, wantDiscard: true, wantStatus: fleet.StatusDeleting},
		{name: "destroy before any container", status: fleet.StatusDeleting, ctx: cancelledCtx(), wantErr: true, wantStatus: fleet.StatusDeleting},
		{name: "shutdown keeps a new container", status: fleet.StatusCreating, ctx: cancelledCtx(), containerID: "cid-1", wantErr: true, wantStatus: fleet.StatusFailed, wantContainer: "cid-1"},
		{name: "shutdown keeps a rebuilt codespace", status: fleet.StatusRebuilding, prior: "cs-1", ctx: cancelledCtx(), containerID: "cs-1", wantErr: true, wantStatus: fleet.StatusFailed, wantContainer: "cs-1"},
		{name: "shutdown before any container keeps the old one", status: fleet.StatusRebuilding, prior: "old", ctx: cancelledCtx(), wantErr: true, wantStatus: fleet.StatusFailed, wantContainer: "old"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seedInstance(t, &fleet.Instance{Name: "i1", Status: tc.status, ContainerID: tc.prior})
			b := &downRecorder{}
			err := stopIfCancelled(tc.ctx, b, "alpha", "i1", tc.containerID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error=%v", err, tc.wantErr)
			}
			if err != nil && (!errors.Is(err, errCancelled) || !errors.Is(err, errTestDestroyed)) {
				t.Fatalf("err = %v, want the cancellation and its cause", err)
			}
			if discarded := len(b.downs) == 1 && b.downs[0] == tc.containerID; discarded != tc.wantDiscard || len(b.downs) > 1 {
				t.Fatalf("downs = %v, want discard=%v", b.downs, tc.wantDiscard)
			}
			inst := loadInstance(t, "i1")
			if inst.Status != tc.wantStatus || inst.ContainerID != tc.wantContainer {
				t.Fatalf("record = %s %q, want %s %q", inst.Status, inst.ContainerID, tc.wantStatus, tc.wantContainer)
			}
			if tc.wantStatus == fleet.StatusFailed && inst.Error != err.Error() {
				t.Fatalf("recorded error = %q, want %q", inst.Error, err.Error())
			}
		})
	}
}

// TestCancelledOr: a step that failed because the run was cancelled (a killed
// git clone or copy) reports the cancellation; any other failure is its own.
func TestCancelledOr(t *testing.T) {
	stepErr := errors.New("signal: killed")
	if err := cancelledOr(context.Background(), stepErr); err != stepErr {
		t.Fatalf("live run: err = %v, want the step's error", err)
	}
	if err := cancelledOr(cancelledCtx(), stepErr); !errors.Is(err, errCancelled) || !errors.Is(err, errTestDestroyed) {
		t.Fatalf("cancelled run: err = %v, want the cancellation and its cause", err)
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
