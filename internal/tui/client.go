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
	"github.com/BenjaminBenetti/fleet-man/internal/protoconv"
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
			Backend:  protoconv.BackendToProto(backendType),
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
			Settings: protoconv.FleetSettingsToProto(s),
		})
		return err
	})
}

// triggerLogsRemote fetches a trigger's recorded event logs (its recent
// firings' payloads, concatenated) from the daemon, for the 'L' pager.
var triggerLogsRemote = func(fleetName, triggerName string) (string, error) {
	var out string
	err := mutate(func(ctx context.Context, svc fleetgrpc.FleetServiceClient) error {
		reply, err := svc.TriggerLogs(ctx, &fleetgrpc.TriggerLogsRequest{Fleet: fleetName, Trigger: triggerName})
		if err != nil {
			return err
		}
		out = reply.GetLogs()
		return nil
	})
	return out, err
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
		_, err := svc.SetConfig(ctx, &fleetgrpc.SetConfigRequest{Config: protoconv.ConfigToProto(c)})
		return err
	})
}

// --- Read path: server snapshot -> legacy render model -----------------------
//
// The TUI still renders from the legacy *configutil.State / *configutil.Config,
// but it SOURCES them from the server (GetState/GetConfig + the Watch stream)
// rather than reading state.json/config.json off disk. The proto<->legacy
// mapping itself lives in internal/protoconv (shared with the server and CLI);
// it retires with the legacy structs in P5.

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
	return protoconv.StateFromProto(reply.GetState()), nil
}

// fetchConfigLegacy pulls the config from the server and converts it. The
// DefaultConfig base means absent optional fields render as their defaults
// (unlike the server's SetConfig write path, which starts from a zero Config).
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
	return protoconv.ConfigFromProto(reply.GetConfig(), configutil.DefaultConfig()), nil
}
