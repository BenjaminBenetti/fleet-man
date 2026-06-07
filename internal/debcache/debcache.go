// Package debcache manages a per-fleet shared apt-cacher-ng server and points
// every instance in the fleet at it so repeated `apt install` reuse downloaded
// .deb packages instead of re-fetching them. When a fleet enables the
// DebCacheServer setting, fleet-man ensures ONE apt-cacher-ng container per
// fleet whose on-disk cache lives under ~/.fleet/workspaces/<fleet>/.aptcache/,
// joins it (and every instance) to the fleet's shared docker network
// (internal/fleetnet), and writes an apt http-proxy config inside each instance
// pointing at the cache by container name.
//
// It is the network-based sibling of internal/buildkit: same host-side `docker`
// orchestration and same best-effort contract (a missing/unwritable apt inside
// an instance is a silent no-op, not an error), but reached over a docker
// network instead of a bind-mounted socket because apt-cacher-ng is an HTTP
// service. Only HTTP apt sources are cached (the default Debian/Ubuntu
// archives); HTTPS sources bypass the proxy and go direct.
//
// Gated to backends that report SupportsCustomMounts()==true (devcontainer);
// callers must check that before invoking EnsureSharedServer.
package debcache

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetnet"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// execCommand builds the host command. Package var so tests can stub the docker
// binary when exercising the real runDocker path.
var execCommand = exec.Command

const (
	// image is the apt-cacher-ng image used for the shared server. There is no
	// official apt-cacher-ng image; sameersbn's is the de-facto standard. A
	// single const keeps it swappable. "latest" trades a first-use pull for
	// simplicity, matching internal/buildkit.
	image = "sameersbn/apt-cacher-ng:latest"

	// proxyPort is the TCP port apt-cacher-ng listens on (its default) and the
	// port instances point their apt http proxy at.
	proxyPort = "3142"

	// containerCachePath is apt-cacher-ng's on-disk cache inside the container;
	// bind-mounted to <.aptcache> on the host so the cache survives container
	// churn and is removable with the fleet.
	containerCachePath = "/var/cache/apt-cacher-ng"

	// fleetNameMaxLen caps the sanitized fleet segment of the container name.
	fleetNameMaxLen = 40
)

// runDocker runs `docker <args...>` and returns its combined output (trimmed)
// and error. Package var so tests can stub the docker interaction without a
// daemon (mirrors internal/buildkit.runDocker).
var runDocker = func(args ...string) (string, error) {
	out, err := backend.NewCmd(execCommand("docker", args...), nil).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ensureNetwork is the fleet-network seam (package var so tests can exercise the
// ensure path without a docker daemon). Mirrors internal/buildkit's seams.
var ensureNetwork = fleetnet.EnsureNetwork

// ensureLocks serializes EnsureSharedServer per fleet so concurrent instance
// creates in the same fleet don't both `docker run` the one shared container and
// have the loser fail on a name conflict.
var ensureLocks sync.Map // fleetName -> *sync.Mutex

func fleetLock(fleetName string) *sync.Mutex {
	m, _ := ensureLocks.LoadOrStore(fleetName, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// ContainerName is the docker container name for a fleet's shared deb cache.
// Sanitized so arbitrary fleet names yield a valid name; it doubles as the
// network DNS name instances use to reach the cache.
func ContainerName(fleetName string) string {
	return "fleet-" + backend.SanitizeName(fleetName, fleetNameMaxLen) + "-aptcache"
}

// ProxyURL is the apt http proxy URL an instance uses to reach the fleet's deb
// cache — the container name (resolved via the fleet network's DNS) plus the
// apt-cacher-ng port.
func ProxyURL(fleetName string) string {
	return "http://" + ContainerName(fleetName) + ":" + proxyPort
}

// SharedDir returns the validated host .aptcache directory for a fleet. It
// refuses fleet names that would let the path escape the workspaces root, since
// EnsureSharedServer creates it and DeleteCache wipes its contents.
func SharedDir(fleetName string) (string, error) {
	if fleetName == "" || strings.ContainsAny(fleetName, `/\`) || strings.Contains(fleetName, "..") {
		return "", fmt.Errorf("invalid fleet name %q for deb cache dir", fleetName)
	}
	return state.DebCacheDir(fleetName), nil
}

// dockerRunArgs builds the `docker run` argv for a fleet's deb cache server.
// Pure (no side effects) so it can be asserted in tests. The container joins the
// fleet network so instances reach it by name, bind-mounts its cache to the host
// so it survives churn, and uses restart=unless-stopped so a docker daemon
// restart brings the cache back — while still being removable via `docker rm -f`.
func dockerRunArgs(fleetName, network string) []string {
	return []string{
		"run", "-d",
		"--name", ContainerName(fleetName),
		"--network", network,
		"--restart", "unless-stopped",
		"-v", state.DebCacheDir(fleetName) + ":" + containerCachePath,
		image,
	}
}

// EnsureSharedServer makes the fleet's shared apt-cacher-ng container exist and
// run, returning the host .aptcache directory. It is idempotent and safe to call
// on every instance create/start: a running container is reused, a stopped one
// is started, an absent one is created — all on the fleet's shared network.
//
// Callers MUST only invoke this for backends that SupportsCustomMounts.
func EnsureSharedServer(fleetName string) (string, error) {
	lock := fleetLock(fleetName)
	lock.Lock()
	defer lock.Unlock()

	dir, err := SharedDir(fleetName)
	if err != nil {
		return "", err
	}
	if err := ensureDir(dir); err != nil {
		return "", fmt.Errorf("create deb cache dir: %w", err)
	}
	network, err := ensureNetwork(fleetName)
	if err != nil {
		return "", fmt.Errorf("ensure fleet network: %w", err)
	}

	name := ContainerName(fleetName)
	running, exists := inspectState(name)
	switch {
	case exists && running:
		flog.Info("deb cache server reused", "fleet", fleetName, "container", name)
	case exists && !running:
		if out, err := runDocker("start", name); err != nil {
			return "", fmt.Errorf("start deb cache server: %w (%s)", err, out)
		}
		flog.Info("deb cache server started", "fleet", fleetName, "container", name)
	default:
		if out, err := runDocker(dockerRunArgs(fleetName, network)...); err != nil {
			// A concurrent (cross-process) or manual create may have won the
			// name race; if the container exists now, treat it as success.
			if _, exists2 := inspectState(name); !exists2 {
				return "", fmt.Errorf("run deb cache server: %w (%s)", err, out)
			}
			flog.Info("deb cache server already present", "fleet", fleetName, "container", name)
		} else {
			flog.Info("deb cache server created", "fleet", fleetName, "container", name)
		}
	}
	return dir, nil
}

// StopSharedServer force-removes a fleet's deb cache container. Best-effort: an
// already-absent container is treated as success so fleet teardown never fails.
func StopSharedServer(fleetName string) error {
	name := ContainerName(fleetName)
	out, err := runDocker("rm", "-f", name)
	if err != nil {
		if strings.Contains(out, "No such container") {
			return nil
		}
		return fmt.Errorf("remove deb cache server %s: %w (%s)", name, err, out)
	}
	flog.Info("deb cache server removed", "fleet", fleetName, "container", name)
	return nil
}

// DeleteCache wipes a fleet's deb cache and restarts the server empty: it
// removes the container, clears the .aptcache directory's CONTENTS (keeping the
// directory itself), then re-ensures the server so it comes back with a fresh
// cache. Serialized per fleet and idempotent. Mirrors internal/buildkit.DeleteCache.
func DeleteCache(fleetName string) error {
	var wipeErr error
	func() {
		lock := fleetLock(fleetName)
		lock.Lock()
		defer lock.Unlock()
		name := ContainerName(fleetName)
		if out, err := runDocker("rm", "-f", name); err != nil && !strings.Contains(out, "No such container") {
			wipeErr = fmt.Errorf("stop deb cache server: %w (%s)", err, out)
			return
		}
		wipeErr = clearSharedDir(fleetName)
	}()

	// Always bring the server back, even on a partial wipe: the container was
	// already removed above, so skipping the restart would leave no cache server.
	_, ensureErr := EnsureSharedServer(fleetName)
	if wipeErr != nil {
		return fmt.Errorf("clear deb cache: %w", wipeErr)
	}
	if ensureErr != nil {
		return fmt.Errorf("restart deb cache server after cache clear: %w", ensureErr)
	}
	flog.Info("deb cache cleared", "fleet", fleetName)
	return nil
}

// clearSharedDir wipes the CONTENTS of a fleet's .aptcache directory but keeps
// the directory itself. apt-cacher-ng writes its cache as its own in-container
// UID, which can leave subdirectories a host-context rm can't traverse; on
// failure it falls back to a throwaway root container that clears the contents.
func clearSharedDir(fleetName string) error {
	dir, err := SharedDir(fleetName)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cleared := true
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			cleared = false
			break
		}
	}
	if cleared {
		return nil
	}
	// Non-host-removable (foreign-UID) entries → clear the contents (NOT the
	// dir) via a throwaway root container, preserving the .aptcache inode.
	if out, err := runDocker("run", "--rm", "--user", "0", "--entrypoint", "find", "-v", dir+":/work", image,
		"/work", "-mindepth", "1", "-maxdepth", "1", "-exec", "rm", "-rf", "{}", ";"); err != nil {
		return fmt.Errorf("clear deb cache dir %s: %w (%s)", dir, err, out)
	}
	return nil
}

// inspectState reports whether a container exists and whether it is running.
func inspectState(name string) (running, exists bool) {
	out, err := runDocker("inspect", "-f", "{{.State.Running}}", name)
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(out) == "true", true
}

// ensureDir creates path 0777 (and re-chmods an existing one), matching the
// mount resolver's convention: bind-mounted into containers whose user UID
// differs from the host's. The dir lives under the user's private ~/.fleet tree.
func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0777); err != nil {
		return err
	}
	return os.Chmod(path, 0777)
}
