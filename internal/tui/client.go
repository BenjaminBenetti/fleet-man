package tui

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc"
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

// dialMutation lazily dials (and caches) the shared mutation connection.
func dialMutation(ctx context.Context) (*fleetclient.Conn, error) {
	mutConnMu.Lock()
	defer mutConnMu.Unlock()
	if mutConn == nil {
		conn, err := fleetclient.Dial(ctx)
		if err != nil {
			return nil, err
		}
		mutConn = conn
	}
	return mutConn, nil
}

// mutate dials (or reuses) the mutation connection and runs fn against the
// service client under a bounded context.
func mutate(fn func(context.Context, fleetgrpc.FleetServiceClient) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), mutationTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return err
	}
	return fn(ctx, conn.Service())
}

// jobStream is the client view of a server job's event stream.
type jobStream = grpc.ServerStreamingClient[fleetgrpc.JobEvent]

// awaitJobStart opens a job stream and returns once the mandatory JobStarted
// arrives (which the server emits only AFTER it has pre-created the record), so
// a pre-create rejection (AlreadyExists / NotFound) surfaces synchronously. The
// job then runs detached server-side; the caller tracks it via reload() +
// pollCreating + the Watch stream. The stream is cancelled on return — that only
// detaches THIS watcher, it does not stop the job.
func awaitJobStart(open func(context.Context, fleetgrpc.FleetServiceClient) (jobStream, error)) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return err
	}
	stream, err := open(ctx, conn.Service())
	if err != nil {
		return err
	}
	_, err = stream.Recv() // JobStarted, or the pre-create error
	return err
}

// awaitJobDone opens a job stream and drains it to the terminal JobDone,
// returning the failure error (if any) and non-fatal warnings. Used for the
// fast lifecycle jobs (start / stop / destroy) where the TUI waits for the
// result before refreshing.
func awaitJobDone(open func(context.Context, fleetgrpc.FleetServiceClient) (jobStream, error)) ([]string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := open(ctx, conn.Service())
	if err != nil {
		return nil, err
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		if d := ev.GetDone(); d != nil {
			if !d.GetSuccess() {
				return d.GetWarnings(), fmt.Errorf("%s", d.GetError())
			}
			return d.GetWarnings(), nil
		}
	}
}

// createInstanceRemote dispatches a CreateInstance job and returns once it has
// started (record pre-created server-side).
var createInstanceRemote = func(fleetName, instanceName, remote, branch string, backendType fleet.BackendType) error {
	return awaitJobStart(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (jobStream, error) {
		req := &fleetgrpc.CreateInstanceRequest{
			Fleet:    fleetName,
			Instance: instanceName,
			Backend:  backendStringToProto(string(backendType)),
		}
		if remote != "" {
			req.Remote = &remote
		}
		if branch != "" {
			req.Branch = &branch
		}
		return svc.CreateInstance(ctx, req)
	})
}

// cloneInstanceRemote dispatches a CloneInstance job (the server copies the
// source's config/backend/tag/color/branch) and returns once it has started.
var cloneInstanceRemote = func(fleetName, srcInstance, destInstance string) error {
	return awaitJobStart(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (jobStream, error) {
		return svc.CloneInstance(ctx, &fleetgrpc.CloneInstanceRequest{
			Fleet:          fleetName,
			SourceInstance: srcInstance,
			NewInstance:    destInstance,
		})
	})
}

// startInstanceRemote / stopInstanceRemote run a fast lifecycle job to
// completion.
var startInstanceRemote = func(fleetName, instanceName string) error {
	_, err := awaitJobDone(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (jobStream, error) {
		return svc.StartInstance(ctx, &fleetgrpc.StartInstanceRequest{Fleet: fleetName, Instance: instanceName})
	})
	return err
}

var stopInstanceRemote = func(fleetName, instanceName string) error {
	_, err := awaitJobDone(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (jobStream, error) {
		return svc.StopInstance(ctx, &fleetgrpc.StopInstanceRequest{Fleet: fleetName, Instance: instanceName})
	})
	return err
}

// destroyInstanceRemote tears down one instance (destroyFleet=false) or the
// whole fleet (destroyFleet=true), to completion.
var destroyInstanceRemote = func(fleetName, instanceName string, destroyFleet bool) error {
	_, err := awaitJobDone(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (jobStream, error) {
		req := &fleetgrpc.DestroyInstanceRequest{Fleet: fleetName, DestroyFleet: destroyFleet}
		if instanceName != "" {
			req.Instance = &instanceName
		}
		return svc.DestroyInstance(ctx, req)
	})
	return err
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
