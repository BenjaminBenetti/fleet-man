package fleetgrpc

// This file holds hand-written PURE domain methods reattached to the generated
// proto types — the "proto-types-are-the-model" pattern. Per the architecture,
// methods here MUST stay pure (no disk, no docker, no network), because client
// packages import fleetgrpc; anything impure belongs in internal/server.
//
// Method names must not collide with the generated field getters (e.g. the
// optional `display_name` field already produces GetDisplayName()), hence
// DisplayNameOrName below rather than GetDisplayName.

// Display returns the lowercase human/CLI form of the status (e.g. "running").
// This is also the legacy state.json string form, so the persistence mapper can
// reuse it later. Returns "unknown" for the unspecified zero value.
func (s InstanceStatus) Display() string {
	switch s {
	case InstanceStatus_INSTANCE_STATUS_CREATING:
		return "creating"
	case InstanceStatus_INSTANCE_STATUS_CLONING:
		return "cloning"
	case InstanceStatus_INSTANCE_STATUS_RUNNING:
		return "running"
	case InstanceStatus_INSTANCE_STATUS_STOPPED:
		return "stopped"
	case InstanceStatus_INSTANCE_STATUS_FAILED:
		return "failed"
	case InstanceStatus_INSTANCE_STATUS_STOPPING:
		return "stopping"
	case InstanceStatus_INSTANCE_STATUS_STARTING:
		return "starting"
	case InstanceStatus_INSTANCE_STATUS_DELETING:
		return "deleting"
	default:
		return "unknown"
	}
}

// Display returns the lowercase backend name (e.g. "devcontainer"), matching the
// CLI/JSON form. Empty for the unspecified zero value.
func (b BackendType) Display() string {
	switch b {
	case BackendType_BACKEND_TYPE_DEVCONTAINER:
		return "devcontainer"
	case BackendType_BACKEND_TYPE_CODER:
		return "coder"
	case BackendType_BACKEND_TYPE_CODESPACES:
		return "codespaces"
	default:
		return ""
	}
}

// DisplayNameOrName returns DisplayName when set, else Name. Legacy instances
// persisted before DisplayName existed fall back to Name. Named to avoid
// colliding with the generated GetDisplayName() field getter.
func (i *Instance) DisplayNameOrName() string {
	if i.GetDisplayName() == "" {
		return i.GetName()
	}
	return i.GetDisplayName()
}

// FindInstance returns the named instance and true, or nil and false when no
// instance with that name exists in the fleet.
func (f *Fleet) FindInstance(name string) (*Instance, bool) {
	for _, inst := range f.GetInstances() {
		if inst.GetName() == name {
			return inst, true
		}
	}
	return nil, false
}
