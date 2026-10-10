package protoconv

import (
	"testing"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// TestJobStepsMatchTheWire pins every domain step to its proto member by name,
// so the shared numbering can't drift, and walks the GENERATED enum so a step
// added to the .proto without a domain constant and a label fails here.
func TestJobStepsMatchTheWire(t *testing.T) {
	want := map[fleet.JobStep]string{
		fleet.StepUnspecified:        "JOB_STEP_UNSPECIFIED",
		fleet.StepValidateBackend:    "JOB_STEP_VALIDATE_BACKEND",
		fleet.StepBuildBackend:       "JOB_STEP_BUILD_BACKEND",
		fleet.StepGitClone:           "JOB_STEP_GIT_CLONE",
		fleet.StepResolveMounts:      "JOB_STEP_RESOLVE_MOUNTS",
		fleet.StepControlMount:       "JOB_STEP_CONTROL_MOUNT",
		fleet.StepBackendUp:          "JOB_STEP_BACKEND_UP",
		fleet.StepStageFleetLaunch:   "JOB_STEP_STAGE_FLEET_LAUNCH",
		fleet.StepStageFleetRC:       "JOB_STEP_STAGE_FLEET_RC",
		fleet.StepApplyMountSymlinks: "JOB_STEP_APPLY_MOUNT_SYMLINKS",
		fleet.StepInstallDotfiles:    "JOB_STEP_INSTALL_DOTFILES",
		fleet.StepRunStartupScripts:  "JOB_STEP_RUN_STARTUP_SCRIPTS",
		fleet.StepInstallClaudeHooks: "JOB_STEP_INSTALL_CLAUDE_HOOKS",
		fleet.StepCopyWorkspace:      "JOB_STEP_COPY_WORKSPACE",
		fleet.StepBackendClone:       "JOB_STEP_BACKEND_CLONE",
		fleet.StepBackendDown:        "JOB_STEP_BACKEND_DOWN",
		fleet.StepRemoveWorkspace:    "JOB_STEP_REMOVE_WORKSPACE",
		fleet.StepBackendStart:       "JOB_STEP_BACKEND_START",
		fleet.StepMarkRunning:        "JOB_STEP_MARK_RUNNING",
		fleet.StepDone:               "JOB_STEP_DONE",
		fleet.StepImageBuild:         "JOB_STEP_IMAGE_BUILD",
		fleet.StepImageDeliver:       "JOB_STEP_IMAGE_DELIVER",
		fleet.StepApplyManifests:     "JOB_STEP_APPLY_MANIFESTS",
		fleet.StepWaitPod:            "JOB_STEP_WAIT_POD",
		fleet.StepSeed:               "JOB_STEP_SEED",
		fleet.StepLifecycleHooks:     "JOB_STEP_LIFECYCLE_HOOKS",
		fleet.StepExportWorkspace:    "JOB_STEP_EXPORT_WORKSPACE",
		fleet.StepRollback:           "JOB_STEP_ROLLBACK",
		fleet.StepConfigureCaches:    "JOB_STEP_CONFIGURE_CACHES",
	}
	for step, name := range want {
		if got := JobStepToProto(step).String(); got != name {
			t.Errorf("domain step %d goes on the wire as %s, want %s", step, got, name)
		}
		if JobStepFromProto(JobStepToProto(step)) != step {
			t.Errorf("step %s does not survive the round trip", name)
		}
	}
	for num, name := range fleetgrpc.JobStep_name {
		step := JobStepFromProto(fleetgrpc.JobStep(num))
		if _, ok := want[step]; !ok {
			t.Errorf("proto step %s has no domain constant", name)
		}
		if step != fleet.StepUnspecified && step.Label() == "" {
			t.Errorf("proto step %s has no label", name)
		}
	}
	if fleet.StepUnspecified.Label() != "" {
		t.Error("the unspecified step should have no label")
	}
	if JobStepFromProto(fleetgrpc.JobStep(4242)).Label() != "" {
		t.Error("a step this build does not know should have no label")
	}
}
