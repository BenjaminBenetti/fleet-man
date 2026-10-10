package tui

import (
	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
)

// job_steps.go shows the step a provisioning job is on, next to the status of
// its instance's row ("creating · cloning repository"). The daemon pushes every
// in-flight job's summary over Watch (ActiveJobs) whenever one starts, changes
// step or ends, so the step is live for jobs any client started, local or
// remote.

// activeJobsMsg carries the daemon's pushed set of in-flight jobs.
type activeJobsMsg struct {
	jobs *fleetgrpc.ActiveJobs
	gen  int
}

// jobStepsFrom indexes the jobs that have reported a step by their instance
// (rtKey). A clone's job is filed under the new instance.
func jobStepsFrom(jobs *fleetgrpc.ActiveJobs) map[string]fleet.JobStep {
	steps := make(map[string]fleet.JobStep, len(jobs.GetJobs()))
	for _, j := range jobs.GetJobs() {
		if step := protoconv.JobStepFromProto(j.GetCurrentStep()); step != fleet.StepUnspecified {
			steps[rtKey(j.GetFleet(), j.GetInstance())] = step
		}
	}
	return steps
}

// jobStepSuffix is what a transitional row shows after its status: the step
// its provisioning job is on, or "" when none is known. Only a provisioning
// row (creating, cloning, rebuilding) shows one: a destroy that cancelled a
// create waits for it to stop, and its deleting row must not read as if the
// create's step ("starting container") were the deletion's.
func (m *model) jobStepSuffix(fleetName string, instance *fleet.Instance) string {
	switch instance.Status {
	case fleet.StatusCreating, fleet.StatusCloning, fleet.StatusRebuilding:
	default:
		return ""
	}
	label := m.jobSteps[rtKey(fleetName, instance.Name)].Label()
	if label == "" {
		return ""
	}
	return dimStyle.Render(" · " + label)
}
