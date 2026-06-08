package instanceops

import (
	"fmt"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backendutil"
	"github.com/BenjaminBenetti/fleet-man/internal/buildkit"
	"github.com/BenjaminBenetti/fleet-man/internal/debcache"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetnet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/imagecache"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// StopInstance transitions the named instance to StatusStopped, stopping its
// container if it is currently running.
func StopInstance(fleetName, instanceName string) (*Result, error) {
	return transitionInstance(fleetName, instanceName, fleet.StatusStopped)
}

// StartInstance transitions the named instance to StatusRunning, starting its
// container and running the post-start hook if it is currently stopped.
func StartInstance(fleetName, instanceName string) (*Result, error) {
	return transitionInstance(fleetName, instanceName, fleet.StatusRunning)
}

// ToggleInstance flips the named instance between running and stopped based on
// its current status, erroring for statuses that have no meaningful toggle.
func ToggleInstance(fleetName, instanceName string) (*Result, error) {
	st, _, instance, err := LoadInstance(fleetName, instanceName)
	if err != nil {
		return nil, err
	}

	switch instance.Status {
	case fleet.StatusRunning, fleet.StatusStopping:
		return transitionLoadedInstance(st, instance, fleetName, instanceName, fleet.StatusStopped)
	case fleet.StatusStopped, fleet.StatusStarting:
		return transitionLoadedInstance(st, instance, fleetName, instanceName, fleet.StatusRunning)
	default:
		return nil, fmt.Errorf("instance %s/%s cannot be toggled from status %q", fleetName, instanceName, instance.Status)
	}
}

func transitionInstance(fleetName, instanceName string, targetStatus fleet.InstanceStatus) (*Result, error) {
	st, _, instance, err := LoadInstance(fleetName, instanceName)
	if err != nil {
		return nil, err
	}

	return transitionLoadedInstance(st, instance, fleetName, instanceName, targetStatus)
}

func transitionLoadedInstance(st *state.State, instance *fleet.Instance, fleetName, instanceName string, targetStatus fleet.InstanceStatus) (*Result, error) {
	result := &Result{
		FleetName:      fleetName,
		InstanceName:   instanceName,
		PreviousStatus: instance.Status,
		Status:         instance.Status,
	}

	if instance.Status == targetStatus {
		return result, nil
	}

	if instance.ContainerID == "" {
		return nil, fmt.Errorf("instance %s/%s has no container ID", fleetName, instanceName)
	}

	instanceBackend := newClient(instance.Backend)

	start := time.Now()
	switch targetStatus {
	case fleet.StatusStopped:
		if instance.Status != fleet.StatusRunning && instance.Status != fleet.StatusStopping {
			return nil, fmt.Errorf("instance %s/%s cannot be stopped from status %q", fleetName, instanceName, instance.Status)
		}
		if err := instanceBackend.Stop(instance.ContainerID); err != nil {
			return nil, fmt.Errorf("stop instance %s/%s: %w", fleetName, instanceName, err)
		}
	case fleet.StatusRunning:
		if instance.Status != fleet.StatusStopped && instance.Status != fleet.StatusStarting {
			return nil, fmt.Errorf("instance %s/%s cannot be started from status %q", fleetName, instanceName, instance.Status)
		}
		if err := instanceBackend.Start(instance.ContainerID); err != nil {
			return nil, fmt.Errorf("start instance %s/%s: %w", fleetName, instanceName, err)
		}
		// Resolve the fleet's persisted container-home for the hook; empty
		// is fine — fleetlaunch substitutes its own default.
		var homeDir string
		if f, ok := st.Fleets[fleetName]; ok {
			homeDir = f.Settings.HomeDir
			// Re-ensure the fleet's shared buildkit server in case the host
			// rebooted (or it was manually removed) while this instance was
			// stopped. Gated to local-docker backends (those that honor custom
			// mounts); cloud backends can't reach a host docker daemon. The
			// instance's own buildx config persists across stop/start, so only
			// the server container needs reviving. Best-effort: a failure just
			// means no shared cache this session.
			execBackend := backendutil.New(instance.Backend, false)
			supportsMounts := execBackend.SupportsCustomMounts()
			if f.Settings.BuildkitServer && supportsMounts {
				if _, err := buildkit.EnsureSharedServer(fleetName); err != nil {
					state.WriteWarn(fleetName, instanceName, fmt.Sprintf("buildkit server: %v", err))
				}
			}
			// Deb/image caches: re-ensure the server AND reconnect the instance to
			// the fleet network (the in-instance apt/docker config persists across
			// stop/start, so only the server + network membership need reviving).
			// The two steps are independent: ConnectInstance ensures the network
			// itself and is idempotent, so a transient server-ensure failure must
			// not skip the (cheap, best-effort) reconnect.
			if f.Settings.DebCacheServer && supportsMounts {
				if _, err := debcache.EnsureSharedServer(fleetName); err != nil {
					state.WriteWarn(fleetName, instanceName, fmt.Sprintf("deb cache: %v", err))
				}
				if err := fleetnet.ConnectInstance(fleetName, instance.ContainerID); err != nil {
					state.WriteWarn(fleetName, instanceName, fmt.Sprintf("deb cache network: %v", err))
				}
			}
			if f.Settings.ImageCacheServer && supportsMounts {
				if _, err := imagecache.EnsureSharedServer(fleetName); err != nil {
					state.WriteWarn(fleetName, instanceName, fmt.Sprintf("image cache: %v", err))
				}
				if err := fleetnet.ConnectInstance(fleetName, instance.ContainerID); err != nil {
					state.WriteWarn(fleetName, instanceName, fmt.Sprintf("image cache network: %v", err))
				}
				// Re-point the instance's dockerd at the mirror on start too: the
				// daemon.json write can be missed at create time (dind not up yet)
				// or lost with the image, and the on-start path above only revives
				// the server + network. Same sync-then-background poll as create;
				// the merge is idempotent so re-running is safe.
				imagecache.ConfigureInstanceDockerPolling(execBackend, instance.WorkspaceDir,
					imagecache.MirrorURL(fleetName), imagecache.MirrorHostPort(fleetName),
					func(configured bool, err error) {
						switch {
						case err != nil:
							state.WriteWarn(fleetName, instanceName, fmt.Sprintf("image cache: %v", err))
						case !configured:
							flog.Info("image cache: no in-instance dockerd appeared within poll window; mirror not configured",
								"fleet", fleetName, "instance", instanceName)
						}
					})
			}
		}
		postStartHook(fleetName, instance, homeDir)
	default:
		return nil, fmt.Errorf("unsupported target status %q", targetStatus)
	}

	instance.Status = targetStatus
	if targetStatus == fleet.StatusRunning {
		instance.Error = ""
	}

	if err := state.Save(st); err != nil {
		return nil, err
	}

	result.Status = instance.Status
	result.Changed = true
	flog.Info("instance status changed", "fleet", fleetName, "instance", instanceName, "from", result.PreviousStatus, "to", targetStatus, "ms", flog.MillisSince(start))
	return result, nil
}

// LoadInstance loads the global state, looks up the named fleet, and resolves
// the named instance within it. It returns the loaded state, the owning fleet,
// and the instance so callers can read or mutate and persist them. It errors
// with "fleet %q not found" when the fleet is absent and propagates the
// instance lookup error from Fleet.GetInstance.
func LoadInstance(fleetName, instanceName string) (*state.State, *fleet.Fleet, *fleet.Instance, error) {
	st, err := state.Load()
	if err != nil {
		return nil, nil, nil, err
	}

	f, ok := st.Fleets[fleetName]
	if !ok {
		return nil, nil, nil, fmt.Errorf("fleet %q not found", fleetName)
	}

	instance, err := f.GetInstance(instanceName)
	if err != nil {
		return nil, nil, nil, err
	}

	return st, f, instance, nil
}
