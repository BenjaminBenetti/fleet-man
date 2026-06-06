// Package buildkit manages a per-fleet shared moby/buildkit server and the
// per-instance docker buildx wiring that points every instance in the fleet at
// it. When a fleet enables the BuildkitServer setting, fleet-man ensures ONE
// privileged buildkit container per fleet whose buildkitd unix socket lives
// under ~/.fleet/workspaces/<fleet>/.buildkit/. That directory is bind-mounted
// into every instance, and each instance's docker buildx is configured as a
// "remote" builder talking to the socket — so all instances share a single
// build cache.
//
// Everything here is HOST-side orchestration of `docker` (the buildkit
// container runs on the same docker daemon as the devcontainers) plus a small
// amount of in-container configuration run through a backend's ExecCommand. It
// is deliberately a server-side package (it reaches for internal/state paths);
// the client/TUI only flips the FleetSettings.BuildkitServer bool.
//
// The feature is gated to backends that report SupportsCustomMounts()==true
// (devcontainer); callers must check that before invoking EnsureSharedServer /
// the instance mount. Even then every step is best-effort: a missing
// docker/buildx inside an instance is a silent no-op, not an error.
package buildkit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// execCommand builds the host command. Package var so tests can stub the docker
// binary itself when exercising the real runDocker path.
var execCommand = exec.Command

const (
	// image is the buildkit image used for the shared server. Matches the
	// user-facing example; "latest" keeps it simple at the cost of a pull on
	// first use.
	image = "moby/buildkit:latest"

	// containerMountPath is where the per-fleet .buildkit host directory is
	// bind-mounted, both inside the buildkit container (so buildkitd writes its
	// socket there) and inside every instance (so buildx can reach the socket).
	// Using the SAME absolute path on both sides means the socket URL is
	// identical everywhere.
	containerMountPath = "/run/fleet-buildkit"

	// socketName is the buildkitd unix socket basename. It is created by the
	// buildkit container inside containerMountPath and therefore appears on the
	// host inside the fleet's .buildkit directory via the bind mount.
	socketName = "buildkitd.sock"

	// containerSocketPath is the absolute socket path inside any container that
	// mounts the .buildkit dir at containerMountPath.
	containerSocketPath = containerMountPath + "/" + socketName

	// containerCachePath is where buildkitd stores its content-addressable
	// cache inside the buildkit container; bind-mounted to <.buildkit>/cache on
	// the host so the cache survives container churn and is removable with the
	// fleet.
	containerCachePath = "/var/lib/buildkit"

	// cacheSubdir is the host subdirectory (under .buildkit) holding the cache.
	cacheSubdir = "cache"

	// builderName is the docker buildx builder created inside each instance. It
	// is local to the instance's docker config; since one instance belongs to
	// exactly one fleet, a fixed name is unambiguous and makes the configure
	// step idempotent (remove-then-create).
	builderName = "fleet-shared"

	// fleetNameMaxLen caps the sanitized fleet segment of the container name.
	fleetNameMaxLen = 40

	// socketWaitTimeout bounds how long EnsureSharedServer waits for buildkitd
	// to create its socket after the container starts before giving up on the
	// permission fix-up (which is best-effort anyway).
	socketWaitTimeout = 15 * time.Second
)

// runDocker runs `docker <args...>` and returns its combined output (trimmed)
// and error. It is a package var so tests can stub the docker interaction
// without a daemon. The real implementation intentionally bypasses
// backend.ExecCommand's event-log entry: these are host-side daemon calls, not
// in-container execs.
var runDocker = func(args ...string) (string, error) {
	out, err := backend.NewCmd(execCommand("docker", args...), nil).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// waitForSocket blocks until path exists or timeout elapses. Package var so
// tests can skip the real poll. Returns an error only on timeout.
var waitForSocket = func(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("buildkit socket %s did not appear within %s", path, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ensureLocks serializes EnsureSharedServer per fleet so concurrent instance
// creates in the same fleet don't both `docker run` the one shared container and
// have the loser fail on a name conflict (losing its socket mount). Keyed per
// fleet so a slow first-time image pull for one fleet never blocks instance
// creation in another.
var ensureLocks sync.Map // fleetName -> *sync.Mutex

func fleetLock(fleetName string) *sync.Mutex {
	m, _ := ensureLocks.LoadOrStore(fleetName, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// ContainerName is the docker container name for a fleet's shared buildkit
// server. Sanitized so arbitrary fleet names yield a valid, collision-resistant
// docker name; the fleet/-buildkit affixes namespace it away from instance
// containers and other fleets.
func ContainerName(fleetName string) string {
	return "fleet-" + backend.SanitizeName(fleetName, fleetNameMaxLen) + "-buildkit"
}

// SharedDir returns the validated host .buildkit directory for a fleet. It
// refuses fleet names that would let the path escape the workspaces root:
// EnsureSharedServer creates this path (os.MkdirAll, 0777) and teardown removes
// it (os.RemoveAll), so a traversing name like "../../x" must never reach those
// calls. Container names go through SanitizeName separately, so this guards the
// filesystem side specifically.
func SharedDir(fleetName string) (string, error) {
	if fleetName == "" || strings.ContainsAny(fleetName, `/\`) || strings.Contains(fleetName, "..") {
		return "", fmt.Errorf("invalid fleet name %q for buildkit dir", fleetName)
	}
	return state.BuildkitDir(fleetName), nil
}

// InstanceMount returns the bind mount that exposes the fleet's buildkit socket
// directory inside an instance. Callers append it to the mounts passed to
// Backend.Up/Clone (only when the backend SupportsCustomMounts).
func InstanceMount(fleetName string) backend.Mount {
	return backend.Mount{
		LocalPath:     state.BuildkitDir(fleetName),
		ContainerPath: containerMountPath,
	}
}

// hostSocketPath is the buildkitd socket as seen on the host (through the bind
// mount).
func hostSocketPath(fleetName string) string {
	return filepath.Join(state.BuildkitDir(fleetName), socketName)
}

// hostCacheDir is the host cache directory bind-mounted to containerCachePath.
func hostCacheDir(fleetName string) string {
	return filepath.Join(state.BuildkitDir(fleetName), cacheSubdir)
}

// dockerRunArgs builds the `docker run` argv for a fleet's buildkit server. Pure
// (no side effects) so it can be asserted in tests. The container is privileged
// (buildkitd requires it) and uses restart=unless-stopped so a docker daemon
// restart brings the shared cache back without waiting for a new instance —
// while still being removable via `docker rm -f` on fleet destroy.
func dockerRunArgs(fleetName string) []string {
	dir := state.BuildkitDir(fleetName)
	return []string{
		"run", "-d",
		"--name", ContainerName(fleetName),
		"--privileged",
		"--restart", "unless-stopped",
		"-v", dir + ":" + containerMountPath,
		"-v", hostCacheDir(fleetName) + ":" + containerCachePath,
		image,
		"--addr", "unix://" + containerSocketPath,
	}
}

// EnsureSharedServer makes the fleet's shared buildkit container exist and run,
// returning the host .buildkit directory to bind into instances. It is
// idempotent and safe to call on every instance create/start: a running
// container is reused, a stopped one is started, and an absent one is created.
//
// After the container is up it waits for buildkitd to create its socket and
// relaxes the socket permissions to 0666 so an instance's non-root user (whose
// UID differs from the buildkit container's root) can connect through the bind
// mount. The permission fix-up is best-effort — a failure is logged, not
// returned, since the socket may simply be slow to appear.
//
// Callers MUST only invoke this for backends that SupportsCustomMounts; cloud
// backends cannot reach a host docker daemon.
func EnsureSharedServer(fleetName string) (string, error) {
	// Serialize per fleet so two concurrent creates in the same fleet don't both
	// `docker run` the shared container (the loser would fail on a name conflict
	// and silently lose its socket mount).
	lock := fleetLock(fleetName)
	lock.Lock()
	defer lock.Unlock()

	dir, err := SharedDir(fleetName)
	if err != nil {
		return "", err
	}
	if err := ensureDir(dir); err != nil {
		return "", fmt.Errorf("create buildkit dir: %w", err)
	}
	if err := ensureDir(hostCacheDir(fleetName)); err != nil {
		return "", fmt.Errorf("create buildkit cache dir: %w", err)
	}

	name := ContainerName(fleetName)
	running, exists := inspectState(name)
	switch {
	case exists && running:
		flog.Info("buildkit server reused", "fleet", fleetName, "container", name)
	case exists && !running:
		if out, err := runDocker("start", name); err != nil {
			return "", fmt.Errorf("start buildkit server: %w (%s)", err, out)
		}
		flog.Info("buildkit server started", "fleet", fleetName, "container", name)
	default:
		if out, err := runDocker(dockerRunArgs(fleetName)...); err != nil {
			// A concurrent (cross-process) or manual create may have won the
			// name race; if the container exists now, treat it as success and
			// fall through to the socket wait. Only a genuine failure aborts.
			if _, exists2 := inspectState(name); !exists2 {
				return "", fmt.Errorf("run buildkit server: %w (%s)", err, out)
			}
			flog.Info("buildkit server already present", "fleet", fleetName, "container", name)
		} else {
			flog.Info("buildkit server created", "fleet", fleetName, "container", name)
		}
	}

	// All paths converge here: wait for buildkitd to create its socket, then
	// relax its permissions so an instance's non-root user can connect. The
	// reuse path waits too — a "running" container may have just been revived by
	// the restart policy and not yet recreated the socket. Non-fatal: the mount
	// still works once the socket appears; instance buildx config fails soft
	// until then.
	if err := waitForSocket(hostSocketPath(fleetName), socketWaitTimeout); err != nil {
		flog.Warn("buildkit socket not ready", "fleet", fleetName, "err", err)
		return dir, nil
	}
	ensureSocketPerms(fleetName, name)
	return dir, nil
}

// StopSharedServer force-removes a fleet's buildkit container. Best-effort: an
// already-absent container is treated as success so fleet teardown never fails
// on a missing server.
func StopSharedServer(fleetName string) error {
	name := ContainerName(fleetName)
	out, err := runDocker("rm", "-f", name)
	if err != nil {
		if strings.Contains(out, "No such container") {
			return nil
		}
		return fmt.Errorf("remove buildkit server %s: %w (%s)", name, err, out)
	}
	flog.Info("buildkit server removed", "fleet", fleetName, "container", name)
	return nil
}

// DeleteCache wipes a fleet's shared build cache and restarts the server empty:
// it stops/removes the buildkit container, deletes the .buildkit directory
// (including buildkitd's root-owned cache), then re-ensures the server so it
// comes back with a fresh cache. Serialized per fleet and idempotent.
//
// The stop+remove happen under the per-fleet lock so a concurrent instance
// create can't start a server we're about to wipe; the re-ensure runs after the
// lock is released (EnsureSharedServer takes it again).
func DeleteCache(fleetName string) error {
	if err := func() error {
		lock := fleetLock(fleetName)
		lock.Lock()
		defer lock.Unlock()
		name := ContainerName(fleetName)
		if out, err := runDocker("rm", "-f", name); err != nil && !strings.Contains(out, "No such container") {
			return fmt.Errorf("stop buildkit server: %w (%s)", err, out)
		}
		return removeSharedDir(fleetName)
	}(); err != nil {
		return err
	}
	flog.Info("buildkit cache cleared", "fleet", fleetName)
	if _, err := EnsureSharedServer(fleetName); err != nil {
		return fmt.Errorf("restart buildkit server after cache clear: %w", err)
	}
	return nil
}

// removeSharedDir deletes a fleet's .buildkit directory. A plain os.RemoveAll
// works when nothing root-owned is present; buildkitd's cache (written as root
// in the privileged container) defeats a user-context rm, so on failure it
// falls back to a throwaway root container that removes the dir.
func removeSharedDir(fleetName string) error {
	dir, err := SharedDir(fleetName)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	parent := filepath.Dir(dir)
	base := filepath.Base(dir)
	if out, err := runDocker("run", "--rm", "--entrypoint", "rm", "-v", parent+":/work", image, "-rf", "/work/"+base); err != nil {
		return fmt.Errorf("remove buildkit dir %s: %w (%s)", dir, err, out)
	}
	return nil
}

// inspectState reports whether a container exists and whether it is running.
// `docker inspect` exits non-zero for a missing container, which we map to
// exists=false.
func inspectState(name string) (running, exists bool) {
	out, err := runDocker("inspect", "-f", "{{.State.Running}}", name)
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(out) == "true", true
}

// ensureSocketPerms relaxes the buildkitd socket to 0666 from inside the
// buildkit container (which runs as root). Best-effort and logged: the call is
// idempotent and a transient failure is retried on the next EnsureSharedServer.
func ensureSocketPerms(fleetName, name string) {
	if out, err := runDocker("exec", name, "chmod", "0666", containerSocketPath); err != nil {
		flog.Warn("buildkit socket chmod failed", "fleet", fleetName, "err", err, "out", out)
	}
}

// ensureDir creates path 0777 (and re-chmods an existing one), matching the
// mount resolver's convention: these dirs are bind-mounted into containers
// whose user UID differs from the host's, so they must be world-writable. They
// already live under the user's private ~/.fleet tree.
func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0777); err != nil {
		return err
	}
	return os.Chmod(path, 0777)
}
