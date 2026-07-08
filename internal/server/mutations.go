package server

import (
	"context"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mutations.go implements the synchronous (non-job) state mutations from the
// FleetService contract. Each is a fast load→apply→save cycle serialized by
// s.muWrite so concurrent mutations can't lost-update each other. On success the
// new snapshot is pushed to the hub (so Watch subscribers see the change
// immediately instead of waiting up to one state-poller tick) and returned in
// the MutationReply so the caller stays consistent without a follow-up GetState.
//
// CONTRACT NOTE: none of these write InstanceStatus — transitional statuses are
// owned by server-side jobs (Phase 4), by design, so a stray client mutation
// can't re-introduce the issue #63 race.

// mutate runs apply against a freshly-loaded State under the write lock, persists
// the result, and broadcasts it. apply must return an already-coded
// status.Error on failure (it is returned to the client verbatim).
func (s *service) mutate(apply func(*state.State) error) (*fleetgrpc.State, error) {
	var snapshot *fleetgrpc.State
	// state.Update holds the state lock across the whole load->apply->save, so
	// these sync mutations are mutually serialized AND serialized against the
	// provisioning jobs (which write via the same state.Update) — no interleaved
	// lost updates within the server process (the issue #63 class).
	err := state.Update(func(st *state.State) error {
		if err := apply(st); err != nil {
			return err
		}
		snapshot = protoconv.StateToProto(st)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Push the new snapshot onto the hub loop so Watch subscribers don't wait for
	// the next state-poller tick. Best-effort: if the hub has stopped (shutdown)
	// the mutation still persisted, so we don't fail the RPC.
	s.hub.post(func(h *hub) { h.setState(snapshot) })
	return snapshot, nil
}

// CreateFleet adds (or returns the existing) fleet — GetOrCreateFleet semantics.
func (s *service) CreateFleet(_ context.Context, req *fleetgrpc.CreateFleetRequest) (*fleetgrpc.MutationReply, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "fleet name required")
	}
	snapshot, err := s.mutate(func(st *state.State) error {
		st.GetOrCreateFleet(req.GetName(), req.GetRemote())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &fleetgrpc.MutationReply{State: snapshot}, nil
}

// DestroyFleet removes a fleet RECORD. It rejects fleets that still have
// instances (those must be torn down via DestroyInstance first) so the record
// never outlives — or orphans — live containers. Removing an already-absent
// fleet is a no-op success (idempotent).
func (s *service) DestroyFleet(_ context.Context, req *fleetgrpc.DestroyFleetRequest) (*fleetgrpc.MutationReply, error) {
	// Read the buildkit setting BEFORE the record is deleted. DestroyFleet only
	// removes an EMPTY fleet, so its shared buildkit server (kept alive by the
	// earlier single-instance destroys) would otherwise be orphaned here — the
	// destroy_fleet job path tears it down, but this synchronous path must too.
	// The deb/image cache + network teardown below is unconditional (idempotent),
	// so a cache toggled off before destroy can't orphan its container/network.
	buildkitEnabled := false
	existed := false
	if st, err := state.Load(); err == nil {
		if f, ok := st.Fleets[req.GetName()]; ok {
			existed = true
			buildkitEnabled = f.Settings.BuildkitServer
		}
	}

	snapshot, err := s.mutate(func(st *state.State) error {
		f, ok := st.Fleets[req.GetName()]
		if !ok {
			return nil
		}
		if len(f.Instances) > 0 {
			return status.Errorf(codes.FailedPrecondition,
				"fleet %q still has %d instance(s); destroy them before removing the fleet", req.GetName(), len(f.Instances))
		}
		delete(st.Fleets, req.GetName())
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Best-effort fleet-level cache teardown (no warning channel on this
	// synchronous RPC, so surface failures to the event log). The network is
	// removed last, after the cache containers, so docker doesn't refuse it for
	// active endpoints.
	teardownWarnings := teardownFleetBuildkit(req.GetName(), buildkitEnabled)
	teardownWarnings = append(teardownWarnings, teardownFleetDebCache(req.GetName(), true)...)
	teardownWarnings = append(teardownWarnings, teardownFleetImageCache(req.GetName(), true)...)
	teardownWarnings = append(teardownWarnings, teardownFleetNetwork(req.GetName(), true)...)
	for _, w := range teardownWarnings {
		flog.Warn("destroy fleet cache teardown", "fleet", req.GetName(), "warn", w)
	}
	if existed {
		flog.Info("fleet destroyed", "fleet", req.GetName())
	}
	return &fleetgrpc.MutationReply{State: snapshot}, nil
}

// SetFleetSettings replaces a fleet's settings wholesale (the caller sends the
// full FleetSettings, preserving tri-state presence such as PreferFleetLaunch).
func (s *service) SetFleetSettings(_ context.Context, req *fleetgrpc.SetFleetSettingsRequest) (*fleetgrpc.MutationReply, error) {
	settings := protoconv.FleetSettingsFromProto(req.GetSettings())
	// Authoritative validation of user-supplied custom-mount paths: the TUI
	// validates for UX, but the server is the trust boundary — a custom mount
	// path becomes a host filesystem segment, so reject traversal/escape here
	// before it ever reaches state.json. Normalize (clean + dedup) the list so
	// what we persist is canonical.
	normalizedMounts, err := fleet.NormalizeCustomMounts(settings.CustomMounts)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid custom mount: %v", err)
	}
	settings.CustomMounts = normalizedMounts
	// Same trust boundary for layout presets: reject empty/duplicate names and
	// pane-less presets here so a malformed list never reaches state.json.
	normalizedPresets, err := fleet.NormalizeLayoutPresets(settings.LayoutPresets)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid layout preset: %v", err)
	}
	settings.LayoutPresets = normalizedPresets
	// Automation (issue #188): validate agents first so trigger validation can
	// resolve the agent names each trigger references. Both are the server's
	// trust boundary before the lists reach state.json.
	normalizedAgents, err := fleet.NormalizeAgents(settings.Agents)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid automation agent: %v", err)
	}
	settings.Agents = normalizedAgents
	normalizedTriggers, err := fleet.NormalizeTriggers(settings.Triggers, settings.Agents)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid automation trigger: %v", err)
	}
	settings.Triggers = normalizedTriggers
	// Coder settings (issue #221): the workspace-name override becomes part of
	// every `coder create <name>` this fleet runs, so validate it here too.
	if err := fleet.NormalizeCoderSettings(&settings); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid coder settings: %v", err)
	}

	snapshot, err := s.mutate(func(st *state.State) error {
		f, ok := st.Fleets[req.GetFleet()]
		if !ok {
			return status.Errorf(codes.NotFound, "fleet %q not found", req.GetFleet())
		}
		f.Settings = settings
		return nil
	})
	if err != nil {
		return nil, err
	}
	flog.Info("fleet settings updated", "fleet", req.GetFleet())
	return &fleetgrpc.MutationReply{State: snapshot}, nil
}

// SetInstanceMetadata updates user-facing labels (display name, color, tag)
// without touching resources. Only the fields present in the request are
// changed; an explicit empty string (e.g. clearing a tag) is distinguished from
// "leave unchanged" by the proto presence bit. A non-empty display name (the
// rename path) must satisfy fleet.ValidateInstanceName; an empty one clears
// the override so the instance falls back to its immutable Name.
func (s *service) SetInstanceMetadata(_ context.Context, req *fleetgrpc.SetInstanceMetadataRequest) (*fleetgrpc.MutationReply, error) {
	if req.DisplayName != nil && req.GetDisplayName() != "" {
		if err := fleet.ValidateInstanceName(req.GetDisplayName()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	snapshot, err := s.mutate(func(st *state.State) error {
		f, ok := st.Fleets[req.GetFleet()]
		if !ok {
			return status.Errorf(codes.NotFound, "fleet %q not found", req.GetFleet())
		}
		inst, err := f.GetInstance(req.GetInstance())
		if err != nil {
			return status.Errorf(codes.NotFound, "instance %q not found in fleet %q", req.GetInstance(), req.GetFleet())
		}
		if req.DisplayName != nil {
			inst.DisplayName = req.GetDisplayName()
		}
		if req.Color != nil {
			inst.Color = req.GetColor()
		}
		if req.Tag != nil {
			inst.Tag = req.GetTag()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	flog.Info("instance metadata updated", "fleet", req.GetFleet(), "instance", req.GetInstance())
	return &fleetgrpc.MutationReply{State: snapshot}, nil
}

// SetGroupLayout persists a tmux pane layout. The state map key is the composite
// computeGroupKey(instanceName, groupID), which the server derives here from the
// layout's own fields (see groupLayoutKey).
func (s *service) SetGroupLayout(_ context.Context, req *fleetgrpc.SetGroupLayoutRequest) (*fleetgrpc.MutationReply, error) {
	gl := req.GetLayout()
	if gl == nil {
		return nil, status.Error(codes.InvalidArgument, "layout required")
	}
	snapshot, err := s.mutate(func(st *state.State) error {
		if st.GroupLayouts == nil {
			st.GroupLayouts = make(map[string]state.GroupLayout)
		}
		st.GroupLayouts[groupLayoutKey(gl.GetInstanceName(), gl.GetGroupId())] = state.GroupLayout{
			GroupID:      gl.GetGroupId(),
			InstanceName: gl.GetInstanceName(),
			Sessions:     gl.GetSessions(),
			Layout:       gl.GetLayout(),
			PaneCount:    int(gl.GetPaneCount()),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &fleetgrpc.MutationReply{State: snapshot}, nil
}

// DeleteGroupLayout removes a persisted layout. Deleting an absent key is a
// no-op success.
func (s *service) DeleteGroupLayout(_ context.Context, req *fleetgrpc.DeleteGroupLayoutRequest) (*fleetgrpc.MutationReply, error) {
	snapshot, err := s.mutate(func(st *state.State) error {
		delete(st.GroupLayouts, groupLayoutKey(req.GetInstanceName(), req.GetGroupId()))
		return nil
	})
	if err != nil {
		return nil, err
	}
	flog.Info("group layout deleted", "instance", req.GetInstanceName(), "group", req.GetGroupId())
	return &fleetgrpc.MutationReply{State: snapshot}, nil
}

// SetLastSeenVersion records the release-notes version the user has seen.
func (s *service) SetLastSeenVersion(_ context.Context, req *fleetgrpc.SetLastSeenVersionRequest) (*fleetgrpc.MutationReply, error) {
	snapshot, err := s.mutate(func(st *state.State) error {
		st.LastSeenVersion = req.GetVersion()
		return nil
	})
	if err != nil {
		return nil, err
	}
	flog.Info("last seen version set", "version", req.GetVersion())
	return &fleetgrpc.MutationReply{State: snapshot}, nil
}

// groupLayoutKey mirrors the TUI's computeGroupKey(instanceName, groupID): the
// composite state-map key that isolates layouts across instances sharing a
// group id.
func groupLayoutKey(instanceName, groupID string) string {
	return instanceName + "/" + groupID
}
