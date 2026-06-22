package tui

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/configutil"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
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

// dialMutation lazily dials (and caches) the shared mutation connection. The
// actual Dial runs OUTSIDE mutConnMu — Dial can block for seconds (a failing
// remote Hello, or the local spawn/version-restart path) and closeMutationConn
// now runs inside the bubbletea Update loop during an armada switch, so holding
// the lock across Dial would freeze the whole UI. We double-check the cache
// after dialing and, if another goroutine won the race (or a switch closed the
// conn under us), discard our connection and use the installed one.
func dialMutation(ctx context.Context) (*fleetclient.Conn, error) {
	mutConnMu.Lock()
	if mutConn != nil {
		conn := mutConn
		mutConnMu.Unlock()
		return conn, nil
	}
	mutConnMu.Unlock()

	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return nil, err
	}

	mutConnMu.Lock()
	defer mutConnMu.Unlock()
	if mutConn != nil {
		// Lost the race; keep the already-installed conn and drop ours.
		_ = conn.Close()
		return mutConn, nil
	}
	mutConn = conn
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

// rebuildInstanceRemote recreates an instance's container in place, to
// completion. Slower than start/stop (it reprovisions), so it shares the same
// long-lived job stream as create/clone.
var rebuildInstanceRemote = func(fleetName, instanceName string) error {
	_, err := awaitJobDone(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) (jobStream, error) {
		return svc.RebuildInstance(ctx, &fleetgrpc.RebuildInstanceRequest{Fleet: fleetName, Instance: instanceName})
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

// deleteCacheTimeout bounds the buildkit cache-wipe RPC. It is much longer than
// the ordinary mutationTimeout because the server stops the container, removes
// the cache, and restarts the server — which on first run may pull the
// moby/buildkit image and then waits up to ~15s for the new socket. 120s keeps
// the common case from timing out before the server reports success.
const deleteCacheTimeout = 120 * time.Second

// deleteBuildkitCacheRemote asks the server to wipe a fleet's shared build cache
// and restart the (empty) buildkit server. Uses its own longer deadline.
var deleteBuildkitCacheRemote = func(fleetName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), deleteCacheTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return err
	}
	_, err = conn.Service().DeleteBuildkitCache(ctx, &fleetgrpc.DeleteBuildkitCacheRequest{Fleet: fleetName})
	return err
}

// deleteDebCacheRemote asks the server to wipe a fleet's shared deb (apt) cache
// and restart the (empty) server. Uses the same longer deadline as the buildkit
// wipe (the server may pull the cache image and restart the container).
var deleteDebCacheRemote = func(fleetName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), deleteCacheTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return err
	}
	_, err = conn.Service().DeleteDebCache(ctx, &fleetgrpc.DeleteDebCacheRequest{Fleet: fleetName})
	return err
}

// deleteImageCacheRemote asks the server to wipe a fleet's shared docker image
// cache and restart the (empty) server. Same longer deadline as above.
var deleteImageCacheRemote = func(fleetName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), deleteCacheTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return err
	}
	_, err = conn.Service().DeleteImageCache(ctx, &fleetgrpc.DeleteImageCacheRequest{Fleet: fleetName})
	return err
}

// inspectRepoTimeout bounds one InspectRepo RPC. Deliberately MUCH longer than
// mutationTimeout (5s): the server shallow-clones the repo (its own clone
// timeout is 90s) and, when detectHomeDir is set, may shell out to docker and
// pull the devcontainer image to read its USER directive — minutes on a cold
// cache. These calls run inside tea.Cmd goroutines, not the Update loop, so
// the long wait never blocks the UI.
const inspectRepoTimeout = 5 * time.Minute

// inspectRepoRemote asks the SERVER to clone + inspect remoteURL (devcontainer
// presence, and optionally the container home dir). Inspection must run where
// provisioning runs — the daemon's host owns the git credentials and docker
// daemon `fleet up` will use — so the verdict is authoritative for local and
// remote TUIs alike (issue #141 note 5). Package var so tests can stub it.
var inspectRepoRemote = func(remoteURL, branch string, detectHomeDir bool) (*fleetgrpc.InspectRepoReply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), inspectRepoTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return nil, err
	}
	return conn.Service().InspectRepo(ctx, &fleetgrpc.InspectRepoRequest{
		RemoteUrl:     remoteURL,
		Branch:        branch,
		DetectHomeDir: detectHomeDir,
	})
}

// notifyTUIConnectedRemote tells the server a TUI has opened so it can run its
// once-per-open state reconciliation (e.g. re-ensuring configured buildkit
// servers). Fire-and-forget: the server returns immediately and does the work
// in the background, and any error here is irrelevant to the TUI — it is a
// best-effort nudge, sent once per launch (see watch.go).
var notifyTUIConnectedRemote = func() error {
	return mutate(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		_, err := svc.FleetTUIConnected(ctx, &fleetgrpc.FleetTUIConnectedRequest{})
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
var setGroupLayoutRemote = func(gl configutil.GroupLayout) error {
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
var setConfigRemote = func(c *configutil.Config) error {
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
			Backend:      backendStringToProto(string(a.Backend)),
		})
	}
	return out
}

func protoAgentsToLegacy(in []*fleetgrpc.Agent) []fleet.Agent {
	if len(in) == 0 {
		return nil
	}
	out := make([]fleet.Agent, 0, len(in))
	for _, a := range in {
		out = append(out, fleet.Agent{
			Name:         a.GetName(),
			Command:      a.GetCommand(),
			SystemPrompt: a.GetSystemPrompt(),
			Backend:      fleet.BackendType(backendProtoToString(a.GetBackend())),
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

func protoTriggersToLegacy(in []*fleetgrpc.Trigger) []fleet.Trigger {
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

func protoLayoutPresetsToLegacy(in []*fleetgrpc.LayoutPreset) []fleet.LayoutPreset {
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

func configToProto(c *configutil.Config) *fleetgrpc.Config {
	if c == nil {
		return &fleetgrpc.Config{}
	}
	pc := &fleetgrpc.Config{
		General:    &fleetgrpc.GeneralSettings{},
		Agent:      &fleetgrpc.AgentSettings{ToolSelection: string(c.AgentSettings.ToolSelection)},
		Dotfiles:   &fleetgrpc.DotfilesSettings{AutoInstall: c.DotfilesSettings.AutoInstall},
		Coder:      &fleetgrpc.CoderSettings{},
		Codespaces: &fleetgrpc.CodespacesSettings{},
		Browser:    &fleetgrpc.BrowserSettings{},
		RemoteMcp: &fleetgrpc.RemoteMcpSettings{
			Enabled:        c.RemoteMcpSettings.Enabled,
			GatewayUrl:     c.RemoteMcpSettings.GatewayURL,
			FleetEnabled:   c.RemoteMcpSettings.FleetEnabled,
			WebhookEnabled: c.RemoteMcpSettings.WebhookEnabled,
		},
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

// --- Read path: server snapshot -> legacy render model -----------------------
//
// The TUI still renders from the legacy *configutil.State / *configutil.Config, but it now
// SOURCES them from the server (GetState/GetConfig + the Watch stream) rather
// than reading state.json/config.json off disk. These proto->legacy converters
// are the inverse of the ones above + internal/server/convert.go; they retire
// with the legacy structs in P5.

// fetchStateLegacy pulls the authoritative persisted state from the server and
// converts it to the legacy model. Package var so tests can stub it.
var fetchStateLegacy = func() (*configutil.State, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mutationTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return nil, err
	}
	reply, err := conn.Service().GetState(ctx, &fleetgrpc.GetStateRequest{})
	if err != nil {
		return nil, err
	}
	return protoStateToLegacy(reply.GetState()), nil
}

// fetchConfigLegacy pulls the config from the server and converts it.
var fetchConfigLegacy = func() (*configutil.Config, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mutationTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return nil, err
	}
	reply, err := conn.Service().GetConfig(ctx, &fleetgrpc.GetConfigRequest{})
	if err != nil {
		return nil, err
	}
	return protoConfigToLegacy(reply.GetConfig()), nil
}

func protoStateToLegacy(ps *fleetgrpc.State) *configutil.State {
	st := &configutil.State{
		Fleets:       make(map[string]*fleet.Fleet),
		GroupLayouts: make(map[string]configutil.GroupLayout),
	}
	if ps == nil {
		return st
	}
	for name, pf := range ps.GetFleets() {
		st.Fleets[name] = protoFleetToLegacy(pf)
	}
	for key, pgl := range ps.GetGroupLayouts() {
		st.GroupLayouts[key] = configutil.GroupLayout{
			GroupID:      pgl.GetGroupId(),
			InstanceName: pgl.GetInstanceName(),
			Sessions:     pgl.GetSessions(),
			Layout:       pgl.GetLayout(),
			PaneCount:    int(pgl.GetPaneCount()),
		}
	}
	st.LastSeenVersion = ps.GetLastSeenVersion()
	return st
}

func protoFleetToLegacy(pf *fleetgrpc.Fleet) *fleet.Fleet {
	f := &fleet.Fleet{
		Name:      pf.GetName(),
		Remote:    pf.GetRemote(),
		Instances: make([]*fleet.Instance, 0, len(pf.GetInstances())),
	}
	if ps := pf.GetSettings(); ps != nil {
		f.Settings = fleet.FleetSettings{
			ClaudeCodeMount:  ps.GetClaudeCodeMount(),
			CodexMount:       ps.GetCodexMount(),
			GhMount:          ps.GetGhMount(),
			AuggieMount:      ps.GetAuggieMount(),
			BuildkitServer:   ps.GetBuildkitServer(),
			CustomMounts:     ps.GetCustomMounts(),
			DebCacheServer:   ps.GetDebCacheServer(),
			ImageCacheServer: ps.GetImageCacheServer(),
			LayoutPresets:    protoLayoutPresetsToLegacy(ps.GetLayoutPresets()),
			Agents:           protoAgentsToLegacy(ps.GetAgents()),
			Triggers:         protoTriggersToLegacy(ps.GetTriggers()),
			HomeDir:          ps.GetHomeDir(),
		}
		if ps.PreferFleetLaunch != nil {
			v := ps.GetPreferFleetLaunch()
			f.Settings.PreferFleetLaunch = &v
		}
	}
	for _, pi := range pf.GetInstances() {
		f.Instances = append(f.Instances, protoInstanceToLegacy(pi))
	}
	return f
}

func protoInstanceToLegacy(pi *fleetgrpc.Instance) *fleet.Instance {
	inst := &fleet.Instance{
		Name:         pi.GetName(),
		DisplayName:  pi.GetDisplayName(),
		ContainerID:  pi.GetContainerId(),
		Config:       pi.GetConfig(),
		WorkspaceDir: pi.GetWorkspaceDir(),
		Status:       statusProtoToLegacy(pi.GetStatus()),
		Error:        pi.GetError(),
		Backend:      fleet.BackendType(backendProtoToString(pi.GetBackend())),
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

func statusProtoToLegacy(s fleetgrpc.InstanceStatus) fleet.InstanceStatus {
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

// backendProtoToString maps the backend enum to the legacy string ("" for
// UNSPECIFIED / not recorded).
func backendProtoToString(b fleetgrpc.BackendType) string {
	switch b {
	case fleetgrpc.BackendType_BACKEND_TYPE_DEVCONTAINER:
		return string(fleet.BackendDevcontainer)
	case fleetgrpc.BackendType_BACKEND_TYPE_CODER:
		return string(fleet.BackendCoder)
	case fleetgrpc.BackendType_BACKEND_TYPE_CODESPACES:
		return string(fleet.BackendCodespaces)
	default:
		return ""
	}
}

func protoConfigToLegacy(pc *fleetgrpc.Config) *configutil.Config {
	c := configutil.DefaultConfig()
	if pc == nil {
		return c
	}
	if g := pc.GetGeneral(); g != nil {
		if g.TmuxVimKeys != nil {
			v := g.GetTmuxVimKeys()
			c.GeneralSettings.TmuxVimKeys = &v
		}
		if g.ShowHelpText != nil {
			v := g.GetShowHelpText()
			c.GeneralSettings.ShowHelpText = &v
		}
	}
	if a := pc.GetAgent(); a != nil && a.GetToolSelection() != "" {
		c.AgentSettings.ToolSelection = configutil.AgentTool(a.GetToolSelection())
	}
	if d := pc.GetDotfiles(); d != nil {
		c.DotfilesSettings.AutoInstall = d.GetAutoInstall()
		c.DotfilesSettings.RepoURL = d.GetRepo()
		c.DotfilesSettings.InstallScript = d.GetInstallScript()
	}
	if cd := pc.GetCoder(); cd != nil {
		c.CoderSettings.Template = cd.GetTemplate()
		c.CoderSettings.Preset = cd.GetPreset()
		for _, p := range cd.GetParameters() {
			c.CoderSettings.Parameters = append(c.CoderSettings.Parameters, configutil.CoderParameter{
				Name:         p.GetName(),
				Value:        p.GetValue(),
				DefaultValue: p.GetDefaultValue(),
				DisplayName:  p.GetDisplayName(),
				Description:  p.GetDescription(),
				Type:         p.GetType(),
			})
		}
	}
	if cs := pc.GetCodespaces(); cs != nil {
		c.CodespacesSettings.Machine = cs.GetMachine()
		c.CodespacesSettings.IdleTimeout = cs.GetIdleTimeout()
		c.CodespacesSettings.DevcontainerPath = cs.GetDevcontainerPath()
	}
	if b := pc.GetBrowser(); b != nil {
		if b.MultipleBrowsersPerFleet != nil {
			v := b.GetMultipleBrowsersPerFleet()
			c.BrowserSettings.MultipleBrowsersPerFleet = &v
		}
		if b.AutoSwitch != nil {
			v := b.GetAutoSwitch()
			c.BrowserSettings.AutoSwitch = &v
		}
	}
	if rm := pc.GetRemoteMcp(); rm != nil {
		c.RemoteMcpSettings.Enabled = rm.GetEnabled()
		c.RemoteMcpSettings.GatewayURL = rm.GetGatewayUrl()
		c.RemoteMcpSettings.FleetEnabled = rm.GetFleetEnabled()
		c.RemoteMcpSettings.WebhookEnabled = rm.GetWebhookEnabled()
	}
	c.DefaultBackend = backendProtoToString(pc.GetDefaultBackend())
	return c
}
