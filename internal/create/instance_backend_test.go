package create

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	coderbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	codespacesbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/codespaces"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// fakeJobBackend stands in for a provisioning backend. It answers the
// capability questions the jobs ask before doing any work, and records an
// UpContext call. Every other method panics on the nil embedded interface, so a
// job that reaches further than a test expects fails loudly.
type fakeJobBackend struct {
	backend.Backend
	clone, rebuild bool
	upErr          error
	upCtx          context.Context
	upReq          backend.UpRequest
	plainUps       int
}

func (f *fakeJobBackend) SupportsSharedMounts() bool { return false }
func (f *fakeJobBackend) SupportsClone() bool        { return f.clone }
func (f *fakeJobBackend) SupportsRebuild() bool      { return f.rebuild }

func (f *fakeJobBackend) Up(string, []backend.Mount) (*backend.UpResult, error) {
	f.plainUps++
	return nil, errors.New("plain Up called on an UpContexter")
}

func (f *fakeJobBackend) UpContext(ctx context.Context, req backend.UpRequest) (*backend.UpResult, error) {
	f.upCtx, f.upReq = ctx, req
	return nil, f.upErr
}

// stubInstanceBackend makes newInstanceBackend return b and record every
// instance record it was asked to bind.
func stubInstanceBackend(t *testing.T, b backend.Backend) *[]fleet.Instance {
	t.Helper()
	var bound []fleet.Instance
	prev := newInstanceBackend
	newInstanceBackend = func(inst *fleet.Instance, _ bool) backend.Backend {
		bound = append(bound, *inst)
		return b
	}
	t.Cleanup(func() { newInstanceBackend = prev })
	return &bound
}

func seedFleet(t *testing.T, instances ...*fleet.Instance) {
	t.Helper()
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Instances: instances},
	}}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

func loadInstance(t *testing.T, name string) *fleet.Instance {
	t.Helper()
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	inst := persistedRecord(st, "alpha", name)
	if inst == nil {
		t.Fatalf("instance %s missing from state", name)
	}
	return inst
}

type ctxMarker struct{}

// TestRunContextBindsCreatingRecordAndProvisionsThroughUpContext pins the create
// construction site: the backend is bound to the CREATING record the server
// pre-wrote (not built from the backend type alone), and provisioning goes
// through UpContext with the job's own ctx, so the JobLog and Hooks the job
// attached reach the backend.
func TestRunContextBindsCreatingRecordAndProvisionsThroughUpContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	created := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	seedFleet(t, &fleet.Instance{
		Name:      "i1",
		Status:    fleet.StatusCreating,
		Backend:   fleet.BackendDevcontainer,
		CreatedAt: created,
		Tag:       "only-on-the-record",
	})
	tmpl := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpl, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, ".devcontainer", "devcontainer.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := &fakeJobBackend{upErr: errors.New("seed failed")}
	bound := stubInstanceBackend(t, fake)
	var jobLog bytes.Buffer
	ctx := backend.WithJobLog(context.WithValue(context.Background(), ctxMarker{}, "job"), &jobLog)

	err := RunContext(ctx, "alpha", "i1", "file://"+tmpl, "", false, fleet.BackendDevcontainer)
	if err == nil || err.Error() != "seed failed" {
		t.Fatalf("RunContext error = %v, want the UpContext failure", err)
	}

	if len(*bound) != 1 {
		t.Fatalf("backend bound %d times, want once", len(*bound))
	}
	rec := (*bound)[0]
	if rec.Name != "i1" || rec.Tag != "only-on-the-record" || !rec.CreatedAt.Equal(created) || rec.Backend != fleet.BackendDevcontainer {
		t.Fatalf("backend bound to %+v, want the persisted CREATING record", rec)
	}

	if fake.plainUps != 0 {
		t.Fatal("RunContext called the plain Up on an UpContexter")
	}
	if fake.upCtx == nil || fake.upCtx.Value(ctxMarker{}) != "job" || backend.JobLogFrom(fake.upCtx) != &jobLog {
		t.Fatal("UpContext did not receive the job's ctx with its JobLog")
	}
	wantWS := filepath.Join(state.WorkspacesDir(), "alpha", "i1", "alpha")
	if fake.upReq.WorkspaceDir != wantWS {
		t.Fatalf("UpContext workspace = %q, want %q", fake.upReq.WorkspaceDir, wantWS)
	}

	if got := loadInstance(t, "i1"); got.Status != fleet.StatusFailed || got.Error != "seed failed" {
		t.Fatalf("record after failed Up = %s %q, want failed with the Up error", got.Status, got.Error)
	}
}

// TestRunCloneBindsTheDestinationRecord pins the clone construction site: the
// backend is bound to the CLONING destination record, not the source, since
// everything the clone creates belongs to the destination.
func TestRunCloneBindsTheDestinationRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedFleet(t,
		&fleet.Instance{Name: "src", Status: fleet.StatusRunning, Backend: fleet.BackendDevcontainer, ContainerID: "c-src", WorkspaceDir: "/ws/src"},
		&fleet.Instance{Name: "dest", Status: fleet.StatusCloning, Backend: fleet.BackendDevcontainer, Tag: "dest-tag"},
	)
	bound := stubInstanceBackend(t, &fakeJobBackend{clone: false})

	err := RunClone(context.Background(), "alpha", "src", "dest", false)
	if err == nil || !strings.Contains(err.Error(), "does not support cloning") {
		t.Fatalf("RunClone error = %v, want the SupportsClone refusal", err)
	}
	if len(*bound) != 1 || (*bound)[0].Name != "dest" || (*bound)[0].Tag != "dest-tag" {
		t.Fatalf("clone backend bound to %+v, want only the destination record", *bound)
	}
	if got := loadInstance(t, "dest"); got.Status != fleet.StatusFailed {
		t.Fatalf("dest status = %s, want failed", got.Status)
	}
	if got := loadInstance(t, "src"); got.Status != fleet.StatusRunning {
		t.Fatalf("src status = %s, want untouched (running)", got.Status)
	}
}

// TestRunRebuildBindsThePersistedRecord pins the rebuild construction site.
func TestRunRebuildBindsThePersistedRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedFleet(t, &fleet.Instance{Name: "i1", Status: fleet.StatusRebuilding, Backend: fleet.BackendDevcontainer, ContainerID: "c1", WorkspaceDir: "/ws/i1"})
	bound := stubInstanceBackend(t, &fakeJobBackend{rebuild: false})

	err := RunRebuild(context.Background(), "alpha", "i1", false)
	if err == nil || !strings.Contains(err.Error(), "does not support rebuild") {
		t.Fatalf("RunRebuild error = %v, want the SupportsRebuild refusal", err)
	}
	if len(*bound) != 1 || (*bound)[0].Name != "i1" || (*bound)[0].ContainerID != "c1" || (*bound)[0].WorkspaceDir != "/ws/i1" {
		t.Fatalf("rebuild backend bound to %+v, want the persisted i1 record", *bound)
	}
}

// TestCreateBackendKeepsCreateTimeBuildersForCloudBackends: coder and
// codespaces need create-only options, so they keep their builders; every
// other backend binds the record through newInstanceBackend.
func TestCreateBackendKeepsCreateTimeBuildersForCloudBackends(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := &fakeJobBackend{}
	bound := stubInstanceBackend(t, fake)

	if _, ok := createBackend(&fleet.Instance{Name: "i1", Backend: fleet.BackendCoder}, "alpha", "git@github.com:o/r.git", "", false).(*coderbackend.CoderBackend); !ok {
		t.Fatal("coder create did not use the coder builder")
	}
	if _, ok := createBackend(&fleet.Instance{Name: "i1", Backend: fleet.BackendCodespaces}, "alpha", "git@github.com:o/r.git", "", false).(*codespacesbackend.CodespacesBackend); !ok {
		t.Fatal("codespaces create did not use the codespaces builder")
	}
	if len(*bound) != 0 {
		t.Fatalf("cloud creates went through newInstanceBackend: %+v", *bound)
	}
	if createBackend(&fleet.Instance{Name: "i1", Backend: fleet.BackendDevcontainer}, "alpha", "", "", false) != backend.Backend(fake) || len(*bound) != 1 {
		t.Fatal("devcontainer create did not bind its record through newInstanceBackend")
	}
}

// TestCreateRecordFallsBackToTheJobArguments: with no persisted record (removed
// while the job started, or no state at all) the create proceeds on a record
// built from its own arguments, and the job's backend type always wins.
func TestCreateRecordFallsBackToTheJobArguments(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	rec := createRecord("alpha", "i1", "/ws", "main", fleet.BackendDevcontainer)
	if rec.Name != "i1" || rec.WorkspaceDir != "/ws" || rec.Branch != "main" || rec.Backend != fleet.BackendDevcontainer || rec.Status != fleet.StatusCreating {
		t.Fatalf("fallback record = %+v", rec)
	}

	seedFleet(t, &fleet.Instance{Name: "i1", Status: fleet.StatusCreating, Tag: "persisted"})
	rec = createRecord("alpha", "i1", "/ws", "", fleet.BackendDevcontainer)
	if rec.Tag != "persisted" || rec.Backend != fleet.BackendDevcontainer {
		t.Fatalf("persisted record = %+v, want the record with the job's backend", rec)
	}
}

// sharedMountsJobBackend is fakeJobBackend for a backend that takes the
// fleet's shared mounts.
type sharedMountsJobBackend struct{ fakeJobBackend }

func (f *sharedMountsJobBackend) SupportsSharedMounts() bool { return true }

// TestRunContextHandsTheBackendTheSharedLayout pins that a backend taking the
// fleet's shared mounts gets the tree the resolver made on the host with them
// (UpRequest.Layout), seeds included: a cluster backend replays it on the
// fleet's share.
func TestRunContextHandsTheBackendTheSharedLayout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{ClaudeCodeMount: true, HomeDir: "/home/node"}, Instances: []*fleet.Instance{
			{Name: "i1", Status: fleet.StatusCreating, Backend: fleet.BackendDevcontainer, CreatedAt: time.Now()},
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	tmpl := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpl, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, ".devcontainer", "devcontainer.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &sharedMountsJobBackend{fakeJobBackend{upErr: errors.New("stop here")}}
	stubInstanceBackend(t, fake)

	if err := RunContext(context.Background(), "alpha", "i1", "file://"+tmpl, "", false, fleet.BackendDevcontainer); err == nil {
		t.Fatal("RunContext succeeded past the failing UpContext")
	}
	want := map[string]backend.LayoutEntry{
		".claude":            {Path: ".claude", Dir: true, Mode: 0o777},
		"files":              {Path: "files", Dir: true, Mode: 0o777},
		"files/.claude.json": {Path: "files/.claude.json", Mode: 0o666, Seed: "{}"},
	}
	got := map[string]backend.LayoutEntry{}
	for _, e := range fake.upReq.Layout {
		got[e.Path] = e
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("Layout entry %s = %+v, want %+v (layout %+v)", p, got[p], w, fake.upReq.Layout)
		}
	}
	if len(fake.upReq.Mounts) == 0 {
		t.Fatal("the backend got no shared mounts")
	}
}
