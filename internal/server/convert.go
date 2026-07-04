package server

import (
	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// convert.go maps the legacy internal/fleet + internal/state structs to the
// fleetgrpc proto types for the wire.
//
// This is TRANSITIONAL glue. The end state (Phase 4/5) is proto-types-are-the-
// model — internal/fleet retired, the server holding *fleetgrpc.State directly,
// and the persistence layer mapping that to/from the legacy state.json bytes.
// Until then, the server reads via state.Load() (legacy types) and converts
// here. The presence rules below mirror the proto field comments: legacy
// `,omitempty` scalars are sent only when non-empty; the non-omitempty legacy
// fields (container_id/config/workspace_dir) are always sent.

func stateToProto(st *state.State) *fleetgrpc.State {
	out := &fleetgrpc.State{
		Fleets:       make(map[string]*fleetgrpc.Fleet, len(st.Fleets)),
		GroupLayouts: make(map[string]*fleetgrpc.GroupLayout, len(st.GroupLayouts)),
	}
	for name, f := range st.Fleets {
		out.Fleets[name] = fleetToProto(f)
	}
	for key, gl := range st.GroupLayouts {
		out.GroupLayouts[key] = groupLayoutToProto(gl)
	}
	if st.LastSeenVersion != "" {
		out.LastSeenVersion = strptr(st.LastSeenVersion)
	}
	return out
}

func fleetToProto(f *fleet.Fleet) *fleetgrpc.Fleet {
	pf := &fleetgrpc.Fleet{
		Name:     f.Name,
		Remote:   f.Remote,
		Settings: fleetSettingsToProto(f.Settings),
	}
	for _, i := range f.Instances {
		pf.Instances = append(pf.Instances, instanceToProto(i))
	}
	return pf
}

func fleetSettingsToProto(s fleet.FleetSettings) *fleetgrpc.FleetSettings {
	ps := &fleetgrpc.FleetSettings{
		ClaudeCodeMount:  s.ClaudeCodeMount,
		CodexMount:       s.CodexMount,
		GhMount:          s.GhMount,
		AuggieMount:      s.AuggieMount,
		BuildkitServer:   s.BuildkitServer,
		CustomMounts:     s.CustomMounts,
		DebCacheServer:   s.DebCacheServer,
		ImageCacheServer: s.ImageCacheServer,
		LayoutPresets:    layoutPresetsToProto(s.LayoutPresets),
		Agents:           agentsToProto(s.Agents),
		Triggers:         triggersToProto(s.Triggers),
		CoderParameters:  coderParametersToProto(s.CoderParameters),
	}
	if s.HomeDir != "" {
		ps.HomeDir = strptr(s.HomeDir)
	}
	if s.PreferFleetLaunch != nil {
		ps.PreferFleetLaunch = boolptr(*s.PreferFleetLaunch)
	}
	if s.CoderTemplate != "" {
		ps.CoderTemplate = strptr(s.CoderTemplate)
	}
	if s.CoderPreset != "" {
		ps.CoderPreset = strptr(s.CoderPreset)
	}
	if s.CoderWorkspaceName != "" {
		ps.CoderWorkspaceName = strptr(s.CoderWorkspaceName)
	}
	return ps
}

// coderParametersToProto maps the fleet's coder parameter bindings to proto
// (nil for an empty list). The template-derived metadata fields travel too so
// a SetFleetSettings round-trip is lossless.
func coderParametersToProto(in []fleet.CoderParameter) []*fleetgrpc.CoderParameter {
	if len(in) == 0 {
		return nil
	}
	out := make([]*fleetgrpc.CoderParameter, 0, len(in))
	for _, p := range in {
		pp := &fleetgrpc.CoderParameter{Name: p.Name, Value: p.Value}
		if p.DefaultValue != "" {
			pp.DefaultValue = strptr(p.DefaultValue)
		}
		if p.DisplayName != "" {
			pp.DisplayName = strptr(p.DisplayName)
		}
		if p.Description != "" {
			pp.Description = strptr(p.Description)
		}
		if p.Type != "" {
			pp.Type = strptr(p.Type)
		}
		out = append(out, pp)
	}
	return out
}

func agentsToProto(in []fleet.Agent) []*fleetgrpc.Agent {
	if len(in) == 0 {
		return nil
	}
	out := make([]*fleetgrpc.Agent, 0, len(in))
	for _, a := range in {
		out = append(out, &fleetgrpc.Agent{
			Name:         a.Name,
			Command:      a.Command,
			SystemPrompt: a.SystemPrompt,
			Backend:      backendToProto(a.Backend),
		})
	}
	return out
}

func triggersToProto(in []fleet.Trigger) []*fleetgrpc.Trigger {
	if len(in) == 0 {
		return nil
	}
	out := make([]*fleetgrpc.Trigger, 0, len(in))
	for _, t := range in {
		out = append(out, &fleetgrpc.Trigger{
			Name:        t.Name,
			Type:        string(t.Type),
			AgentNames:  t.AgentNames,
			Prompt:      t.Prompt,
			Cron:        t.Cron,
			Script:      t.Script,
			WebhookName: t.WebhookName,
			FilterType:  string(t.FilterType),
			Regex:       t.Regex,
			JsonPath:    t.JSONPath,
			JsonValue:   t.JSONValue,
			Disabled:    t.Disabled,
		})
	}
	return out
}

func layoutPresetsToProto(in []fleet.LayoutPreset) []*fleetgrpc.LayoutPreset {
	if len(in) == 0 {
		return nil
	}
	out := make([]*fleetgrpc.LayoutPreset, 0, len(in))
	for _, p := range in {
		out = append(out, &fleetgrpc.LayoutPreset{
			Name:         p.Name,
			Layout:       p.Layout,
			PaneCommands: p.PaneCommands,
		})
	}
	return out
}

func instanceToProto(i *fleet.Instance) *fleetgrpc.Instance {
	pi := &fleetgrpc.Instance{
		Name: i.Name,
		// Non-omitempty legacy fields: always present.
		ContainerId:  strptr(i.ContainerID),
		Config:       strptr(i.Config),
		WorkspaceDir: strptr(i.WorkspaceDir),
		Status:       statusToProto(i.Status),
		Backend:      backendToProto(i.Backend),
	}
	if !i.CreatedAt.IsZero() {
		pi.CreatedAt = timestamppb.New(i.CreatedAt)
	}
	// `,omitempty` legacy scalars: send only when set.
	if i.DisplayName != "" {
		pi.DisplayName = strptr(i.DisplayName)
	}
	if i.Error != "" {
		pi.Error = strptr(i.Error)
	}
	if i.Tag != "" {
		pi.Tag = strptr(i.Tag)
	}
	if i.Color != "" {
		pi.Color = strptr(i.Color)
	}
	if i.Branch != "" {
		pi.Branch = strptr(i.Branch)
	}
	if i.Automated {
		pi.Automated = true
	}
	// Warnings is a new field with no legacy state.json source; left nil here
	// (it gets populated by jobs starting in Phase 4).
	return pi
}

func groupLayoutToProto(gl state.GroupLayout) *fleetgrpc.GroupLayout {
	return &fleetgrpc.GroupLayout{
		GroupId:      gl.GroupID,
		InstanceName: gl.InstanceName,
		Sessions:     gl.Sessions,
		Layout:       gl.Layout,
		PaneCount:    int32(gl.PaneCount),
	}
}

func statusToProto(s fleet.InstanceStatus) fleetgrpc.InstanceStatus {
	switch s {
	case fleet.StatusCreating:
		return fleetgrpc.InstanceStatus_INSTANCE_STATUS_CREATING
	case fleet.StatusCloning:
		return fleetgrpc.InstanceStatus_INSTANCE_STATUS_CLONING
	case fleet.StatusRunning:
		return fleetgrpc.InstanceStatus_INSTANCE_STATUS_RUNNING
	case fleet.StatusStopped:
		return fleetgrpc.InstanceStatus_INSTANCE_STATUS_STOPPED
	case fleet.StatusFailed:
		return fleetgrpc.InstanceStatus_INSTANCE_STATUS_FAILED
	case fleet.StatusStopping:
		return fleetgrpc.InstanceStatus_INSTANCE_STATUS_STOPPING
	case fleet.StatusStarting:
		return fleetgrpc.InstanceStatus_INSTANCE_STATUS_STARTING
	case fleet.StatusDeleting:
		return fleetgrpc.InstanceStatus_INSTANCE_STATUS_DELETING
	case fleet.StatusRebuilding:
		return fleetgrpc.InstanceStatus_INSTANCE_STATUS_REBUILDING
	default:
		return fleetgrpc.InstanceStatus_INSTANCE_STATUS_UNSPECIFIED
	}
}

func backendToProto(b fleet.BackendType) fleetgrpc.BackendType {
	switch b {
	case fleet.BackendDevcontainer:
		return fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER
	case fleet.BackendCoder:
		return fleetgrpc.BackendType_BACKEND_TYPE_CODER
	case fleet.BackendCodespaces:
		return fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES
	default:
		return fleetgrpc.BackendType_BACKEND_TYPE_UNSPECIFIED
	}
}

func strptr(s string) *string { return &s }
func boolptr(b bool) *bool    { return &b }
