package create

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// recordingJobLog is a provisioning-log sink that keeps the output written to
// it and the steps and warnings reported through it, in order.
type recordingJobLog struct {
	mu       sync.Mutex
	out      bytes.Buffer
	steps    []fleet.JobStep
	warnings []string
}

func (r *recordingJobLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out.Write(p)
}

func (r *recordingJobLog) Step(step fleet.JobStep, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
}

func (r *recordingJobLog) Warn(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warnings = append(r.warnings, message)
}

// gitFixtureRepo makes a one-commit git repository to clone from.
func gitFixtureRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".devcontainer", "devcontainer.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "add", "-A"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// TestRunContextReportsStepsAndClonesIntoTheJobLog: a create reports each step
// it enters, in order, and git clone's output reaches the job's log.
func TestRunContextReportsStepsAndClonesIntoTheJobLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedFleet(t, &fleet.Instance{Name: "i1", Status: fleet.StatusCreating, Backend: fleet.BackendDevcontainer})
	repo := gitFixtureRepo(t)
	stubInstanceBackend(t, &fakeJobBackend{upErr: errors.New("up failed")})

	log := &recordingJobLog{}
	err := RunContext(backend.WithJobLog(context.Background(), log), "alpha", "i1", repo, "", false, fleet.BackendDevcontainer)
	if err == nil || err.Error() != "up failed" {
		t.Fatalf("RunContext error = %v, want the Up failure", err)
	}

	want := []fleet.JobStep{fleet.StepBuildBackend, fleet.StepGitClone, fleet.StepResolveMounts, fleet.StepBackendUp}
	if !reflect.DeepEqual(log.steps, want) {
		t.Fatalf("steps = %v, want %v", log.steps, want)
	}
	if !strings.Contains(log.out.String(), "Cloning into") {
		t.Fatalf("git clone output did not reach the job log: %q", log.out.String())
	}
	if _, err := os.Stat(filepath.Join(state.WorkspacesDir(), "alpha", "i1", "alpha", ".devcontainer")); err != nil {
		t.Fatalf("clone did not land: %v", err)
	}
}

// TestRunContextReportsTheTemplateCopy: a template fleet's copy is its own step.
func TestRunContextReportsTheTemplateCopy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedFleet(t, &fleet.Instance{Name: "i1", Status: fleet.StatusCreating, Backend: fleet.BackendDevcontainer})
	tmpl := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpl, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubInstanceBackend(t, &fakeJobBackend{upErr: errors.New("up failed")})

	log := &recordingJobLog{}
	_ = RunContext(backend.WithJobLog(context.Background(), log), "alpha", "i1", "file://"+tmpl, "", false, fleet.BackendDevcontainer)

	want := []fleet.JobStep{fleet.StepBuildBackend, fleet.StepCopyWorkspace, fleet.StepResolveMounts, fleet.StepBackendUp}
	if !reflect.DeepEqual(log.steps, want) {
		t.Fatalf("steps = %v, want %v", log.steps, want)
	}
}

// TestProvisionWarningWritesTheBannerAndTheJobLog: a provisioning warning still
// writes the instance's banner, and now also reaches the job's log.
func TestProvisionWarningWritesTheBannerAndTheJobLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	log := &recordingJobLog{}

	provisionWarning(backend.WithJobLog(context.Background(), log), "alpha", "i1", "deb cache: boom")

	banner, err := os.ReadFile(state.WarnPath("alpha", "i1"))
	if err != nil || string(banner) != "deb cache: boom" {
		t.Fatalf("banner = %q, %v; want the warning", banner, err)
	}
	if !reflect.DeepEqual(log.warnings, []string{"deb cache: boom"}) {
		t.Fatalf("job log warnings = %q, want the warning", log.warnings)
	}
}

// TestBackendWarningsStayOnTopOfTheBanner: the provisioning steps replace the
// banner as they warn, so an Up's own warnings (a Kubernetes create's dropped
// debug grants) are put in front of it after them, the steps' text kept
// below; an Up without warnings leaves the banner as the steps left it.
func TestBackendWarningsStayOnTopOfTheBanner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	provisionWarning(context.Background(), "alpha", "i1", "stage fleet.rc: boom")
	prependBackendWarnings("alpha", "i1", []string{"debug capabilities dropped", " ", "needs a pull login"})
	banner, err := os.ReadFile(state.WarnPath("alpha", "i1"))
	if err != nil || string(banner) != "debug capabilities dropped\nneeds a pull login\nstage fleet.rc: boom" {
		t.Fatalf("banner = %q, %v", banner, err)
	}

	prependBackendWarnings("alpha", "i2", nil)
	if _, err := os.Stat(state.WarnPath("alpha", "i2")); !os.IsNotExist(err) {
		t.Fatalf("an Up without warnings wrote a banner: %v", err)
	}
}

// rebuildingJobBackend rebuilds through RebuildContext, failing with upErr.
type rebuildingJobBackend struct {
	fakeJobBackend
	rebuildCtx context.Context
	rebuildID  string
	rebuildReq backend.UpRequest
}

func (r *rebuildingJobBackend) Rebuild(string, string, []backend.Mount) (*backend.UpResult, error) {
	return nil, errors.New("plain Rebuild called on a RebuildContexter")
}

func (r *rebuildingJobBackend) RebuildContext(ctx context.Context, containerID string, req backend.UpRequest) (*backend.UpResult, error) {
	r.rebuildCtx, r.rebuildID, r.rebuildReq = ctx, containerID, req
	return nil, r.upErr
}

// TestRunRebuildRebuildsThroughRebuildContext: a rebuild hands the job's ctx
// (and so its log) to the backend's RebuildContext, and reports its steps.
func TestRunRebuildRebuildsThroughRebuildContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedFleet(t, &fleet.Instance{Name: "i1", Status: fleet.StatusRebuilding, Backend: fleet.BackendDevcontainer, ContainerID: "old-cid", WorkspaceDir: "/ws/i1"})
	fake := &rebuildingJobBackend{fakeJobBackend: fakeJobBackend{rebuild: true, upErr: errors.New("rebuild failed")}}
	stubInstanceBackend(t, fake)

	log := &recordingJobLog{}
	ctx := backend.WithJobLog(context.Background(), log)
	err := RunRebuild(ctx, "alpha", "i1", false)
	if err == nil || err.Error() != "rebuild failed" {
		t.Fatalf("RunRebuild error = %v, want the RebuildContext failure", err)
	}
	if fake.rebuildCtx == nil || backend.JobLogFrom(fake.rebuildCtx) != log {
		t.Fatal("RebuildContext did not receive the job's ctx with its log")
	}
	if fake.rebuildID != "old-cid" || fake.rebuildReq.WorkspaceDir != "/ws/i1" {
		t.Fatalf("RebuildContext got %q %+v, want the record's container and workspace", fake.rebuildID, fake.rebuildReq)
	}
	want := []fleet.JobStep{fleet.StepResolveMounts, fleet.StepBackendUp}
	if !reflect.DeepEqual(log.steps, want) {
		t.Fatalf("steps = %v, want %v", log.steps, want)
	}
	if got := loadInstance(t, "i1"); got.Status != fleet.StatusFailed || got.Error != "rebuild failed" {
		t.Fatalf("record = %s %q, want failed with the rebuild error", got.Status, got.Error)
	}
}

// TestRunRebuildLeavesACheckpointedRecord: a rebuild the backend left at its
// journal when the daemon shut down keeps its record rebuilding, for the
// next start to undo; the run reports the checkpoint.
func TestRunRebuildLeavesACheckpointedRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedFleet(t, &fleet.Instance{Name: "i1", Status: fleet.StatusRebuilding, Backend: fleet.BackendDevcontainer, ContainerID: "old-cid", WorkspaceDir: "/ws/i1"})
	checkpoint := fmt.Errorf("kubernetes:interrupted: the daemon shut down during the rebuild (at step reset): %w", backend.ErrCheckpointed)
	stubInstanceBackend(t, &rebuildingJobBackend{fakeJobBackend: fakeJobBackend{rebuild: true, upErr: checkpoint}})
	err := RunRebuild(context.Background(), "alpha", "i1", false)
	if !errors.Is(err, backend.ErrCheckpointed) {
		t.Fatalf("err = %v, want the checkpoint", err)
	}
	if got := loadInstance(t, "i1"); got.Status != fleet.StatusRebuilding || got.Error != "" {
		t.Fatalf("record = %s %q, want it left rebuilding", got.Status, got.Error)
	}
}

// checkoutSeeingBackend records whether the workspace was there when the
// rebuild began.
type checkoutSeeingBackend struct {
	rebuildingJobBackend
	sawCheckout bool
}

func (c *checkoutSeeingBackend) RebuildContext(ctx context.Context, containerID string, req backend.UpRequest) (*backend.UpResult, error) {
	_, err := os.Stat(filepath.Join(req.WorkspaceDir, ".devcontainer", "devcontainer.json"))
	c.sawCheckout = err == nil
	return c.rebuildingJobBackend.RebuildContext(ctx, containerID, req)
}

// TestRunRebuildRetriesACreateFromAFreshCheckout: the rebuild of a cluster
// instance that never reached running is the retry of its create, which
// builds from the daemon host's checkout: one that is gone is made again
// from the fleet's remote (here a template) first. A rebuild of an instance
// that ran exports its workspace instead, and of another backend's touches
// nothing.
func TestRunRebuildRetriesACreateFromAFreshCheckout(t *testing.T) {
	for _, tc := range []struct {
		name         string
		inst         fleet.Instance
		wantCheckout bool
	}{
		{"failed cluster create", fleet.Instance{Backend: fleet.BackendKubernetes, Status: fleet.StatusRebuilding, KubeCluster: "c", KubeKey: "abcd2345"}, true},
		{"cluster instance that ran", fleet.Instance{Backend: fleet.BackendKubernetes, Status: fleet.StatusRebuilding, KubeCluster: "c", KubeKey: "abcd2345", ContainerID: "k8s:c/ns/sts@g1"}, false},
		{"docker instance", fleet.Instance{Backend: fleet.BackendDevcontainer, Status: fleet.StatusRebuilding}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			tmpl := t.TempDir()
			if err := os.MkdirAll(filepath.Join(tmpl, ".devcontainer"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tmpl, ".devcontainer", "devcontainer.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			inst := tc.inst
			inst.Name = "i1"
			inst.WorkspaceDir = filepath.Join(state.WorkspacesDir(), "alpha", "i1", "alpha")
			if err := state.Save(&state.State{Fleets: map[string]*fleet.Fleet{
				"alpha": {Name: "alpha", Remote: "file://" + tmpl, Instances: []*fleet.Instance{&inst}},
			}}); err != nil {
				t.Fatal(err)
			}
			fake := &checkoutSeeingBackend{rebuildingJobBackend: rebuildingJobBackend{fakeJobBackend: fakeJobBackend{rebuild: true, upErr: errors.New("stop here")}}}
			stubInstanceBackend(t, fake)
			if err := RunRebuild(context.Background(), "alpha", "i1", false); err == nil || err.Error() != "stop here" {
				t.Fatalf("RunRebuild = %v", err)
			}
			if fake.sawCheckout != tc.wantCheckout {
				t.Fatalf("the checkout was there when the rebuild began: %v, want %v", fake.sawCheckout, tc.wantCheckout)
			}
		})
	}
}
