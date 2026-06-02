package tui

import (
	"context"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// client.go is the TUI's write seam onto the fleet server. The synchronous
// (non-job) state + config mutations the dialogs / settings page used to perform
// with state.Save / state.SaveConfig now go through these RPC wrappers (Phase 3).
//
// They are package vars (mirroring cli.fetchFleetState) so unit tests can stub a
// single mutation without standing up a server. The call sites still mutate the
// in-memory m.st / m.config optimistically for an instant redraw; these wrappers
// just persist the change through the server (the single writer). The read-path
// flip to the server snapshot lands in Phase 4.

// mutationTimeout bounds one synchronous mutation RPC so a wedged server can't
// hang the bubbletea Update loop (which is where these run, same as the old
// synchronous state.Save did).
const mutationTimeout = 5 * time.Second

// A process-wide connection reused for every mutation RPC. The Watch stream
// keeps its own connection (watch.go); this one is dialed lazily on the first
// mutation — by which point the Watch stream has already spawned/handshaked the
// server — and reused thereafter (grpc handles transparent reconnection).
var (
	mutConnMu sync.Mutex
	mutConn   *fleetclient.Conn
)

// mutate dials (or reuses) the mutation connection and runs fn against the
// service client under a bounded context.
func mutate(fn func(context.Context, fleetgrpc.FleetServiceClient) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), mutationTimeout)
	defer cancel()

	mutConnMu.Lock()
	defer mutConnMu.Unlock()

	if mutConn == nil {
		conn, err := fleetclient.Dial(ctx)
		if err != nil {
			return err
		}
		mutConn = conn
	}
	return fn(ctx, mutConn.Service())
}

// closeMutationConn tears down the mutation connection. Called from Run() after
// the program exits so the socket fd is released.
func closeMutationConn() {
	mutConnMu.Lock()
	defer mutConnMu.Unlock()
	if mutConn != nil {
		_ = mutConn.Close()
		mutConn = nil
	}
}

// --- Mutation wrappers (the test seam) -------------------------------------

// createFleetRemote adds (or returns the existing) fleet record.
var createFleetRemote = func(name, remote string) error {
	return mutate(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		_, err := svc.CreateFleet(ctx, &fleetgrpc.CreateFleetRequest{Name: name, Remote: remote})
		return err
	})
}

// destroyFleetRemote removes an (empty) fleet record.
var destroyFleetRemote = func(name string) error {
	return mutate(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		_, err := svc.DestroyFleet(ctx, &fleetgrpc.DestroyFleetRequest{Name: name})
		return err
	})
}

// setFleetSettingsRemote replaces a fleet's settings (full FleetSettings,
// preserving tri-state presence).
var setFleetSettingsRemote = func(fleetName string, s fleet.FleetSettings) error {
	return mutate(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		_, err := svc.SetFleetSettings(ctx, &fleetgrpc.SetFleetSettingsRequest{
			Fleet:    fleetName,
			Settings: fleetSettingsToProto(s),
		})
		return err
	})
}

// setInstanceMetadataRemote updates user-facing labels. A nil field means
// "leave unchanged"; a non-nil pointer (incl. to "") sets the value.
var setInstanceMetadataRemote = func(fleetName, instance string, displayName, color, tag *string) error {
	return mutate(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		_, err := svc.SetInstanceMetadata(ctx, &fleetgrpc.SetInstanceMetadataRequest{
			Fleet:       fleetName,
			Instance:    instance,
			DisplayName: displayName,
			Color:       color,
			Tag:         tag,
		})
		return err
	})
}

// setGroupLayoutRemote persists one tmux pane layout.
var setGroupLayoutRemote = func(gl state.GroupLayout) error {
	return mutate(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		_, err := svc.SetGroupLayout(ctx, &fleetgrpc.SetGroupLayoutRequest{Layout: &fleetgrpc.GroupLayout{
			GroupId:      gl.GroupID,
			InstanceName: gl.InstanceName,
			Sessions:     gl.Sessions,
			Layout:       gl.Layout,
			PaneCount:    int32(gl.PaneCount),
		}})
		return err
	})
}

// deleteGroupLayoutRemote removes one persisted layout.
var deleteGroupLayoutRemote = func(instanceName, groupID string) error {
	return mutate(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		_, err := svc.DeleteGroupLayout(ctx, &fleetgrpc.DeleteGroupLayoutRequest{
			InstanceName: instanceName,
			GroupId:      groupID,
		})
		return err
	})
}

// setLastSeenVersionRemote records the release-notes version the user has seen.
var setLastSeenVersionRemote = func(version string) error {
	return mutate(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		_, err := svc.SetLastSeenVersion(ctx, &fleetgrpc.SetLastSeenVersionRequest{Version: version})
		return err
	})
}

// setConfigRemote replaces the whole config (the settings page sends the full
// edited Config).
var setConfigRemote = func(c *state.Config) error {
	return mutate(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		_, err := svc.SetConfig(ctx, &fleetgrpc.SetConfigRequest{Config: configToProto(c)})
		return err
	})
}

// --- Client-side legacy -> proto converters --------------------------------
//
// These mirror internal/server/convert.go + config.go but live here because the
// client must not import the server. The duplication is transitional: when proto
// becomes the model (Phase 5) the legacy structs — and these mappers — go away.

func fleetSettingsToProto(s fleet.FleetSettings) *fleetgrpc.FleetSettings {
	ps := &fleetgrpc.FleetSettings{
		ClaudeCodeMount: s.ClaudeCodeMount,
		CodexMount:      s.CodexMount,
		GhMount:         s.GhMount,
	}
	if s.HomeDir != "" {
		ps.HomeDir = &s.HomeDir
	}
	if s.PreferFleetLaunch != nil {
		v := *s.PreferFleetLaunch
		ps.PreferFleetLaunch = &v
	}
	return ps
}

func configToProto(c *state.Config) *fleetgrpc.Config {
	if c == nil {
		return &fleetgrpc.Config{}
	}
	pc := &fleetgrpc.Config{
		General:        &fleetgrpc.GeneralSettings{},
		Agent:          &fleetgrpc.AgentSettings{ToolSelection: string(c.AgentSettings.ToolSelection)},
		Dotfiles:       &fleetgrpc.DotfilesSettings{AutoInstall: c.DotfilesSettings.AutoInstall},
		Coder:          &fleetgrpc.CoderSettings{},
		Codespaces:     &fleetgrpc.CodespacesSettings{},
		Browser:        &fleetgrpc.BrowserSettings{},
		DefaultBackend: backendStringToProto(c.DefaultBackend),
	}

	if c.GeneralSettings.TmuxVimKeys != nil {
		v := *c.GeneralSettings.TmuxVimKeys
		pc.General.TmuxVimKeys = &v
	}
	if c.GeneralSettings.ShowHelpText != nil {
		v := *c.GeneralSettings.ShowHelpText
		pc.General.ShowHelpText = &v
	}

	if c.DotfilesSettings.RepoURL != "" {
		pc.Dotfiles.Repo = strp(c.DotfilesSettings.RepoURL)
	}
	if c.DotfilesSettings.InstallScript != "" {
		pc.Dotfiles.InstallScript = strp(c.DotfilesSettings.InstallScript)
	}

	if c.CoderSettings.Template != "" {
		pc.Coder.Template = strp(c.CoderSettings.Template)
	}
	if c.CoderSettings.Preset != "" {
		pc.Coder.Preset = strp(c.CoderSettings.Preset)
	}
	for _, p := range c.CoderSettings.Parameters {
		pp := &fleetgrpc.CoderParameter{Name: p.Name, Value: p.Value}
		if p.DefaultValue != "" {
			pp.DefaultValue = strp(p.DefaultValue)
		}
		if p.DisplayName != "" {
			pp.DisplayName = strp(p.DisplayName)
		}
		if p.Description != "" {
			pp.Description = strp(p.Description)
		}
		if p.Type != "" {
			pp.Type = strp(p.Type)
		}
		pc.Coder.Parameters = append(pc.Coder.Parameters, pp)
	}

	if c.CodespacesSettings.Machine != "" {
		pc.Codespaces.Machine = strp(c.CodespacesSettings.Machine)
	}
	if c.CodespacesSettings.IdleTimeout != "" {
		pc.Codespaces.IdleTimeout = strp(c.CodespacesSettings.IdleTimeout)
	}
	if c.CodespacesSettings.DevcontainerPath != "" {
		pc.Codespaces.DevcontainerPath = strp(c.CodespacesSettings.DevcontainerPath)
	}

	if c.BrowserSettings.MultipleBrowsersPerFleet != nil {
		v := *c.BrowserSettings.MultipleBrowsersPerFleet
		pc.Browser.MultipleBrowsersPerFleet = &v
	}
	if c.BrowserSettings.AutoSwitch != nil {
		v := *c.BrowserSettings.AutoSwitch
		pc.Browser.AutoSwitch = &v
	}

	return pc
}

func backendStringToProto(b string) fleetgrpc.BackendType {
	switch fleet.BackendType(b) {
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

func strp(s string) *string { return &s }
