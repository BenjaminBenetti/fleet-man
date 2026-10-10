package fleet

// JobStep is a step of a lifecycle job: the stage a create, clone or rebuild
// has reached. A provisioning job reports each step it enters, the daemon keeps
// the latest as the job's current step, and the instance's row shows it while
// the job runs. The values are fleetgrpc.JobStep's numbers (protoconv converts
// between the two), so they are fixed on the wire: add steps, never renumber.
type JobStep int32

const (
	StepUnspecified        JobStep = 0
	StepValidateBackend    JobStep = 1
	StepBuildBackend       JobStep = 2
	StepGitClone           JobStep = 3
	StepResolveMounts      JobStep = 4
	StepControlMount       JobStep = 5
	StepBackendUp          JobStep = 6
	StepStageFleetLaunch   JobStep = 7
	StepStageFleetRC       JobStep = 8
	StepApplyMountSymlinks JobStep = 9
	StepInstallDotfiles    JobStep = 10
	StepRunStartupScripts  JobStep = 11
	StepInstallClaudeHooks JobStep = 12
	StepCopyWorkspace      JobStep = 13
	StepBackendClone       JobStep = 14
	StepBackendDown        JobStep = 15
	StepRemoveWorkspace    JobStep = 16
	StepBackendStart       JobStep = 17
	StepMarkRunning        JobStep = 18
	StepDone               JobStep = 19

	// The Kubernetes backend's provisioning steps.
	StepImageBuild      JobStep = 20
	StepImageDeliver    JobStep = 21
	StepApplyManifests  JobStep = 22
	StepWaitPod         JobStep = 23
	StepSeed            JobStep = 24
	StepLifecycleHooks  JobStep = 25
	StepExportWorkspace JobStep = 26
	StepRollback        JobStep = 27

	StepConfigureCaches JobStep = 28
)

// jobStepLabels are the words an instance row shows for each step, lower case
// so they read after the status ("creating · cloning repository").
var jobStepLabels = map[JobStep]string{
	StepValidateBackend:    "checking backend",
	StepBuildBackend:       "preparing backend",
	StepGitClone:           "cloning repository",
	StepResolveMounts:      "preparing mounts",
	StepControlMount:       "preparing control socket",
	StepBackendUp:          "starting container",
	StepStageFleetLaunch:   "installing fleet",
	StepStageFleetRC:       "installing fleet.rc",
	StepApplyMountSymlinks: "linking agent state",
	StepInstallDotfiles:    "installing dotfiles",
	StepRunStartupScripts:  "running startup scripts",
	StepInstallClaudeHooks: "installing agent hooks",
	StepCopyWorkspace:      "copying workspace",
	StepBackendClone:       "cloning container",
	StepBackendDown:        "removing container",
	StepRemoveWorkspace:    "removing workspace",
	StepBackendStart:       "starting",
	StepMarkRunning:        "finishing",
	StepDone:               "done",
	StepImageBuild:         "building image",
	StepImageDeliver:       "delivering image",
	StepApplyManifests:     "applying manifests",
	StepWaitPod:            "waiting for pod",
	StepSeed:               "seeding disk",
	StepLifecycleHooks:     "running lifecycle hooks",
	StepExportWorkspace:    "exporting workspace",
	StepRollback:           "rolling back",
	StepConfigureCaches:    "configuring caches",
}

// Label is the step as an instance row shows it, or "" for StepUnspecified and
// for a step this build does not know (one a newer daemon reports).
func (s JobStep) Label() string {
	return jobStepLabels[s]
}
