// Package protoconv is the single source of truth for mapping between the
// legacy domain model (internal/fleet + internal/state) and the fleetgrpc
// proto wire types.
//
// Before this package existed the same converters were maintained in
// triplicate — internal/server (convert.go/mutations.go/config.go), the TUI
// (internal/tui/client.go) and the CLI (internal/cli/automation.go) — because
// the depguard client boundary forbids clients from importing the server. The
// copies drifted (the CLI's trigger converter lost the Disabled field, so any
// CLI automation write silently re-enabled disabled triggers). Consolidating
// them here removes the "every new field must be added in 6 places" trap: one
// converter per type per direction, exercised by the exhaustive round-trip
// tests in this package.
//
// Clients may import this package: the boundary's intent is to stop clients
// from ACCESSING state/backends (Load/Save/exec), and these are pure functions
// over DTO shapes — no I/O, no state access. That purity is machine-checked,
// not just promised: the legacy struct types are named through the
// internal/configutil aliases, and this package is itself inside the depguard
// client rule, so an import of internal/state (or any server-only package)
// fails the lint.
//
// This is still TRANSITIONAL glue. The end state (Phase 4/5 of the fleetd
// migration) is proto-types-are-the-model, at which point the legacy structs —
// and this package — go away.
//
// Conventions, mirroring the proto field comments:
//   - Legacy `,omitempty` scalars are sent only when non-empty; the
//     non-omitempty legacy fields (container_id/config/workspace_dir) are
//     always sent.
//   - Repeated fields map to nil (not an empty slice) when empty, matching
//     the `,omitempty` JSON tags on the legacy structs.
//   - Tri-state *bool fields preserve nil-vs-set presence in both directions.
package protoconv

import (
	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// --- State ------------------------------------------------------------------

// StateToProto maps the whole persisted state to the wire snapshot.
func StateToProto(st *configutil.State) *fleetgrpc.State {
	out := &fleetgrpc.State{
		Fleets:       make(map[string]*fleetgrpc.Fleet, len(st.Fleets)),
		GroupLayouts: make(map[string]*fleetgrpc.GroupLayout, len(st.GroupLayouts)),
	}
	for name, f := range st.Fleets {
		out.Fleets[name] = FleetToProto(f)
	}
	for key, gl := range st.GroupLayouts {
		out.GroupLayouts[key] = GroupLayoutToProto(gl)
	}
	if st.LastSeenVersion != "" {
		out.LastSeenVersion = strptr(st.LastSeenVersion)
	}
	return out
}

// StateFromProto reconstructs the legacy state from a wire snapshot. The maps
// are always non-nil so callers can index/insert without nil checks.
func StateFromProto(ps *fleetgrpc.State) *configutil.State {
	st := &configutil.State{
		Fleets:       make(map[string]*fleet.Fleet),
		GroupLayouts: make(map[string]configutil.GroupLayout),
	}
	if ps == nil {
		return st
	}
	for name, pf := range ps.GetFleets() {
		st.Fleets[name] = FleetFromProto(pf)
	}
	for key, pgl := range ps.GetGroupLayouts() {
		st.GroupLayouts[key] = GroupLayoutFromProto(pgl)
	}
	st.LastSeenVersion = ps.GetLastSeenVersion()
	return st
}

// --- Fleet / Instance -------------------------------------------------------

// FleetToProto maps one fleet record (settings + instances) to the wire.
func FleetToProto(f *fleet.Fleet) *fleetgrpc.Fleet {
	pf := &fleetgrpc.Fleet{
		Name:     f.Name,
		Remote:   f.Remote,
		Settings: FleetSettingsToProto(f.Settings),
	}
	for _, i := range f.Instances {
		pf.Instances = append(pf.Instances, InstanceToProto(i))
	}
	return pf
}

// FleetFromProto reconstructs one fleet record. Instances is always a non-nil
// slice (possibly empty), matching what the TUI render path expects.
func FleetFromProto(pf *fleetgrpc.Fleet) *fleet.Fleet {
	f := &fleet.Fleet{
		Name:      pf.GetName(),
		Remote:    pf.GetRemote(),
		Settings:  FleetSettingsFromProto(pf.GetSettings()),
		Instances: make([]*fleet.Instance, 0, len(pf.GetInstances())),
	}
	for _, pi := range pf.GetInstances() {
		f.Instances = append(f.Instances, InstanceFromProto(pi))
	}
	return f
}

// InstanceToProto maps one instance record to the wire.
func InstanceToProto(i *fleet.Instance) *fleetgrpc.Instance {
	pi := &fleetgrpc.Instance{
		Name: i.Name,
		// Non-omitempty legacy fields: always present.
		ContainerId:  strptr(i.ContainerID),
		Config:       strptr(i.Config),
		WorkspaceDir: strptr(i.WorkspaceDir),
		Status:       StatusToProto(i.Status),
		Backend:      BackendToProto(i.Backend),
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
	pi.Automated = i.Automated
	// Warnings is proto-only (populated by jobs); it has no legacy field.
	return pi
}

// InstanceFromProto reconstructs one instance record. The proto-only Warnings
// field has no legacy home and is dropped.
func InstanceFromProto(pi *fleetgrpc.Instance) *fleet.Instance {
	inst := &fleet.Instance{
		Name:         pi.GetName(),
		DisplayName:  pi.GetDisplayName(),
		ContainerID:  pi.GetContainerId(),
		Config:       pi.GetConfig(),
		WorkspaceDir: pi.GetWorkspaceDir(),
		Status:       StatusFromProto(pi.GetStatus()),
		Error:        pi.GetError(),
		Backend:      BackendFromProto(pi.GetBackend()),
		Tag:          pi.GetTag(),
		Color:        pi.GetColor(),
		Branch:       pi.GetBranch(),
		Automated:    pi.GetAutomated(),
	}
	if ts := pi.GetCreatedAt(); ts != nil {
		inst.CreatedAt = ts.AsTime()
	}
	return inst
}

// --- FleetSettings ----------------------------------------------------------

// FleetSettingsToProto maps a fleet's settings to the wire. Optional scalars
// travel only when set; PreferFleetLaunch preserves its tri-state.
func FleetSettingsToProto(s fleet.FleetSettings) *fleetgrpc.FleetSettings {
	ps := &fleetgrpc.FleetSettings{
		ClaudeCodeMount:  s.ClaudeCodeMount,
		CodexMount:       s.CodexMount,
		GhMount:          s.GhMount,
		AuggieMount:      s.AuggieMount,
		BuildkitServer:   s.BuildkitServer,
		CustomMounts:     s.CustomMounts,
		DebCacheServer:   s.DebCacheServer,
		ImageCacheServer: s.ImageCacheServer,
		LayoutPresets:    LayoutPresetsToProto(s.LayoutPresets),
		Agents:           AgentsToProto(s.Agents),
		Triggers:         TriggersToProto(s.Triggers),
		CoderParameters:  CoderParametersToProto(s.CoderParameters),
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

// FleetSettingsFromProto reconstructs a fleet's settings (zero value for nil).
func FleetSettingsFromProto(ps *fleetgrpc.FleetSettings) fleet.FleetSettings {
	s := fleet.FleetSettings{}
	if ps == nil {
		return s
	}
	s.ClaudeCodeMount = ps.GetClaudeCodeMount()
	s.CodexMount = ps.GetCodexMount()
	s.GhMount = ps.GetGhMount()
	s.AuggieMount = ps.GetAuggieMount()
	s.BuildkitServer = ps.GetBuildkitServer()
	s.CustomMounts = ps.GetCustomMounts()
	s.DebCacheServer = ps.GetDebCacheServer()
	s.ImageCacheServer = ps.GetImageCacheServer()
	s.LayoutPresets = LayoutPresetsFromProto(ps.GetLayoutPresets())
	s.Agents = AgentsFromProto(ps.GetAgents())
	s.Triggers = TriggersFromProto(ps.GetTriggers())
	s.HomeDir = ps.GetHomeDir()
	if ps.PreferFleetLaunch != nil {
		v := ps.GetPreferFleetLaunch()
		s.PreferFleetLaunch = &v
	}
	s.CoderTemplate = ps.GetCoderTemplate()
	s.CoderPreset = ps.GetCoderPreset()
	s.CoderWorkspaceName = ps.GetCoderWorkspaceName()
	s.CoderParameters = CoderParametersFromProto(ps.GetCoderParameters())
	return s
}

// --- Automation (agents + triggers) ------------------------------------------

// AgentsToProto maps the automation agent list (nil for empty).
func AgentsToProto(in []fleet.Agent) []*fleetgrpc.Agent {
	if len(in) == 0 {
		return nil
	}
	out := make([]*fleetgrpc.Agent, 0, len(in))
	for _, a := range in {
		out = append(out, &fleetgrpc.Agent{
			Name:         a.Name,
			Command:      a.Command,
			SystemPrompt: a.SystemPrompt,
			Backend:      BackendToProto(a.Backend),
		})
	}
	return out
}

// AgentsFromProto reconstructs the automation agent list (nil for empty).
func AgentsFromProto(in []*fleetgrpc.Agent) []fleet.Agent {
	if len(in) == 0 {
		return nil
	}
	out := make([]fleet.Agent, 0, len(in))
	for _, a := range in {
		out = append(out, fleet.Agent{
			Name:         a.GetName(),
			Command:      a.GetCommand(),
			SystemPrompt: a.GetSystemPrompt(),
			Backend:      BackendFromProto(a.GetBackend()),
		})
	}
	return out
}

// TriggersToProto maps the automation trigger list (nil for empty).
func TriggersToProto(in []fleet.Trigger) []*fleetgrpc.Trigger {
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

// TriggersFromProto reconstructs the automation trigger list (nil for empty).
func TriggersFromProto(in []*fleetgrpc.Trigger) []fleet.Trigger {
	if len(in) == 0 {
		return nil
	}
	out := make([]fleet.Trigger, 0, len(in))
	for _, t := range in {
		out = append(out, fleet.Trigger{
			Name:        t.GetName(),
			Type:        fleet.TriggerType(t.GetType()),
			AgentNames:  t.GetAgentNames(),
			Prompt:      t.GetPrompt(),
			Cron:        t.GetCron(),
			Script:      t.GetScript(),
			WebhookName: t.GetWebhookName(),
			FilterType:  fleet.WebhookFilterType(t.GetFilterType()),
			Regex:       t.GetRegex(),
			JSONPath:    t.GetJsonPath(),
			JSONValue:   t.GetJsonValue(),
			Disabled:    t.GetDisabled(),
		})
	}
	return out
}

// --- Layout presets -----------------------------------------------------------

// LayoutPresetsToProto maps the saved layout preset list (nil for empty).
func LayoutPresetsToProto(in []fleet.LayoutPreset) []*fleetgrpc.LayoutPreset {
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

// LayoutPresetsFromProto reconstructs the layout preset list (nil for empty).
func LayoutPresetsFromProto(in []*fleetgrpc.LayoutPreset) []fleet.LayoutPreset {
	if len(in) == 0 {
		return nil
	}
	out := make([]fleet.LayoutPreset, 0, len(in))
	for _, p := range in {
		out = append(out, fleet.LayoutPreset{
			Name:         p.GetName(),
			Layout:       p.GetLayout(),
			PaneCommands: p.GetPaneCommands(),
		})
	}
	return out
}

// --- Coder parameters ---------------------------------------------------------

// CoderParametersToProto maps the coder parameter bindings (nil for empty).
// The template-derived metadata fields travel too so a SetFleetSettings
// round-trip is lossless.
func CoderParametersToProto(in []fleet.CoderParameter) []*fleetgrpc.CoderParameter {
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

// CoderParametersFromProto reconstructs the coder parameter bindings (nil for
// empty).
func CoderParametersFromProto(in []*fleetgrpc.CoderParameter) []fleet.CoderParameter {
	if len(in) == 0 {
		return nil
	}
	out := make([]fleet.CoderParameter, 0, len(in))
	for _, p := range in {
		out = append(out, fleet.CoderParameter{
			Name:         p.GetName(),
			Value:        p.GetValue(),
			DefaultValue: p.GetDefaultValue(),
			DisplayName:  p.GetDisplayName(),
			Description:  p.GetDescription(),
			Type:         p.GetType(),
		})
	}
	return out
}

// --- Group layouts ------------------------------------------------------------

// GroupLayoutToProto maps one persisted tmux pane layout.
func GroupLayoutToProto(gl configutil.GroupLayout) *fleetgrpc.GroupLayout {
	return &fleetgrpc.GroupLayout{
		GroupId:      gl.GroupID,
		InstanceName: gl.InstanceName,
		Sessions:     gl.Sessions,
		Layout:       gl.Layout,
		PaneCount:    int32(gl.PaneCount),
	}
}

// GroupLayoutFromProto reconstructs one persisted tmux pane layout.
func GroupLayoutFromProto(pgl *fleetgrpc.GroupLayout) configutil.GroupLayout {
	return configutil.GroupLayout{
		GroupID:      pgl.GetGroupId(),
		InstanceName: pgl.GetInstanceName(),
		Sessions:     pgl.GetSessions(),
		Layout:       pgl.GetLayout(),
		PaneCount:    int(pgl.GetPaneCount()),
	}
}

// --- Enums ---------------------------------------------------------------------

// StatusToProto maps the legacy instance status to its proto enum
// (UNSPECIFIED for empty/unknown).
func StatusToProto(s fleet.InstanceStatus) fleetgrpc.InstanceStatus {
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

// StatusFromProto maps the proto enum back to the legacy status ("" for
// UNSPECIFIED).
func StatusFromProto(s fleetgrpc.InstanceStatus) fleet.InstanceStatus {
	switch s {
	case fleetgrpc.InstanceStatus_INSTANCE_STATUS_CREATING:
		return fleet.StatusCreating
	case fleetgrpc.InstanceStatus_INSTANCE_STATUS_CLONING:
		return fleet.StatusCloning
	case fleetgrpc.InstanceStatus_INSTANCE_STATUS_RUNNING:
		return fleet.StatusRunning
	case fleetgrpc.InstanceStatus_INSTANCE_STATUS_STOPPED:
		return fleet.StatusStopped
	case fleetgrpc.InstanceStatus_INSTANCE_STATUS_FAILED:
		return fleet.StatusFailed
	case fleetgrpc.InstanceStatus_INSTANCE_STATUS_STOPPING:
		return fleet.StatusStopping
	case fleetgrpc.InstanceStatus_INSTANCE_STATUS_STARTING:
		return fleet.StatusStarting
	case fleetgrpc.InstanceStatus_INSTANCE_STATUS_DELETING:
		return fleet.StatusDeleting
	case fleetgrpc.InstanceStatus_INSTANCE_STATUS_REBUILDING:
		return fleet.StatusRebuilding
	default:
		return ""
	}
}

// BackendToProto maps the legacy backend type to its proto enum (UNSPECIFIED
// for empty/unknown).
func BackendToProto(b fleet.BackendType) fleetgrpc.BackendType {
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

// BackendFromProto maps the proto enum back to the legacy backend type ("" for
// UNSPECIFIED / not recorded; callers that need a default apply their own,
// e.g. NormalizeAgent falls back to devcontainer).
func BackendFromProto(b fleetgrpc.BackendType) fleet.BackendType {
	switch b {
	case fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER:
		return fleet.BackendDevcontainer
	case fleetgrpc.BackendType_BACKEND_TYPE_CODER:
		return fleet.BackendCoder
	case fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES:
		return fleet.BackendCodespaces
	default:
		return ""
	}
}

func strptr(s string) *string { return &s }
func boolptr(b bool) *bool    { return &b }
