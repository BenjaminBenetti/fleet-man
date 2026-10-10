package tui

import (
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/charmbracelet/x/ansi"
)

func TestJobStepsFrom(t *testing.T) {
	steps := jobStepsFrom(&fleetgrpc.ActiveJobs{Jobs: []*fleetgrpc.JobSummary{
		{Fleet: "alpha", Instance: "i1", CurrentStep: fleetgrpc.JobStep_JOB_STEP_GIT_CLONE},
		{Fleet: "alpha", Instance: "copy", CurrentStep: fleetgrpc.JobStep_JOB_STEP_BACKEND_CLONE},
		{Fleet: "alpha", Instance: "starting", Kind: fleetgrpc.JobKind_JOB_KIND_START_INSTANCE},
	}})
	want := map[string]fleet.JobStep{
		rtKey("alpha", "i1"):   fleet.StepGitClone,
		rtKey("alpha", "copy"): fleet.StepBackendClone,
	}
	if len(steps) != len(want) {
		t.Fatalf("steps = %v, want %v (a job without a step is left out)", steps, want)
	}
	for k, v := range want {
		if steps[k] != v {
			t.Errorf("steps[%s] = %v, want %v", k, steps[k], v)
		}
	}
	if got := jobStepsFrom(nil); len(got) != 0 {
		t.Fatalf("jobStepsFrom(nil) = %v, want empty", got)
	}
}

// TestViewFleetListShowsTheProvisioningStep: a transitional row shows the step
// its job is on after the status, and nothing once no step is known.
func TestViewFleetListShowsTheProvisioningStep(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status fleet.InstanceStatus
		steps  map[string]fleet.JobStep
		want   string
	}{
		{name: "creating", status: fleet.StatusCreating, steps: map[string]fleet.JobStep{rtKey("alpha", "agent-1"): fleet.StepGitClone}, want: "creating · cloning repository"},
		{name: "rebuilding", status: fleet.StatusRebuilding, steps: map[string]fleet.JobStep{rtKey("alpha", "agent-1"): fleet.StepWaitPod}, want: "rebuilding · waiting for pod"},
		{name: "no step yet", status: fleet.StatusCreating, want: "creating"},
		{name: "another instance's step", status: fleet.StatusCreating, steps: map[string]fleet.JobStep{rtKey("alpha", "other"): fleet.StepGitClone}, want: "creating"},
		{name: "a step this build does not know", status: fleet.StatusCreating, steps: map[string]fleet.JobStep{rtKey("alpha", "agent-1"): fleet.JobStep(4242)}, want: "creating"},
		{name: "cloning", status: fleet.StatusCloning, steps: map[string]fleet.JobStep{rtKey("alpha", "agent-1"): fleet.StepCopyWorkspace}, want: "cloning · copying workspace"},
		// A destroy waiting on the create it cancelled: the create's step is not the deletion's.
		{name: "deleting during a cancelled create", status: fleet.StatusDeleting, steps: map[string]fleet.JobStep{rtKey("alpha", "agent-1"): fleet.StepBackendUp}, want: "deleting"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			inst := &fleet.Instance{Name: "agent-1", Status: tt.status}
			fp := newFleetPage()
			fp.rows = []row{
				{kind: rowFleetHeader, fleetName: "alpha"},
				{kind: rowInstance, fleetName: "alpha", instance: inst},
			}
			m := &model{
				st:           &state.State{Fleets: map[string]*fleet.Fleet{"alpha": {Name: "alpha", Instances: []*fleet.Instance{inst}}}},
				sessionStore: NewSessionStore(),
				fleetPage:    fp,
				jobSteps:     tt.steps,
			}
			view := ansi.Strip(fp.viewFleetList(m))
			if !strings.Contains(view, tt.want) {
				t.Fatalf("view is missing %q:\n%s", tt.want, view)
			}
			if !strings.Contains(tt.want, "·") && strings.Contains(view, tt.want+" ·") {
				t.Fatalf("view shows a step it should not:\n%s", view)
			}
		})
	}
}

// TestActiveJobsMsgFollowsTheConnection: a push replaces the steps, and one
// from a superseded connection is dropped.
func TestActiveJobsMsgFollowsTheConnection(t *testing.T) {
	m := model{watchGen: 2, jobSteps: map[string]fleet.JobStep{rtKey("alpha", "old"): fleet.StepGitClone}}
	push := &fleetgrpc.ActiveJobs{Jobs: []*fleetgrpc.JobSummary{{Fleet: "alpha", Instance: "i1", CurrentStep: fleetgrpc.JobStep_JOB_STEP_BACKEND_UP}}}

	next, _ := m.Update(activeJobsMsg{jobs: push, gen: 1})
	if got := next.(model).jobSteps; got[rtKey("alpha", "old")] != fleet.StepGitClone || len(got) != 1 {
		t.Fatalf("a stale push changed the steps: %v", got)
	}
	next, _ = next.(model).Update(activeJobsMsg{jobs: push, gen: 2})
	if got := next.(model).jobSteps; got[rtKey("alpha", "i1")] != fleet.StepBackendUp || len(got) != 1 {
		t.Fatalf("steps after the push = %v, want only i1's", got)
	}
}
