// Package imagecache manages a per-fleet shared docker registry pull-through
// cache (a `registry:2` mirror of Docker Hub) and points every instance's own
// docker daemon at it so repeated `docker pull` of docker.io images don't
// re-download. When a fleet enables the ImageCacheServer setting, fleet-man
// ensures ONE registry container per fleet whose storage lives under
// ~/.fleet/workspaces/<fleet>/.imgcache/, joins it (and every instance) to the
// fleet's shared docker network (internal/fleetnet), and writes
// registry-mirrors + insecure-registries into each instance's
// /etc/docker/daemon.json (then SIGHUP-reloads dockerd).
//
// It is the network-based sibling of internal/buildkit: same host-side `docker`
// orchestration and same best-effort contract. Unlike buildkit (which only
// needs the buildx CLI), the consumer here is a docker DAEMON, so the feature
// only helps instances that run their OWN dockerd (docker-in-docker);
// docker-outside-of-docker and no-docker instances are silently skipped. Only
// docker.io is mirrored — that is all docker registry-mirrors ever applies to.
//
// Gated to backends that report SupportsCustomMounts()==true (devcontainer).
package imagecache

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
	// image is the registry image used for the shared pull-through cache. The
	// official registry:2 supports proxy (mirror) mode via REGISTRY_PROXY_*.
	image = "registry:2"

	// upstreamRegistry is the registry the cache mirrors. docker registry-mirrors
	// only ever applies to Docker Hub, so this is fixed to Docker Hub's registry.
	upstreamRegistry = "https://registry-1.docker.io"

	// mirrorPort is the TCP port registry:2 listens on (its default) and the port
	// instances point their registry-mirror at.
	mirrorPort = "5000"

	// containerCachePath is registry:2's storage dir inside the container;
	// bind-mounted to <.imgcache> on the host so the cache survives churn.
	containerCachePath = "/var/lib/registry"

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
// ensure path without a docker daemon).
var ensureNetwork = fleetnet.EnsureNetwork

// ensureLocks serializes EnsureSharedServer per fleet so concurrent instance
// creates in the same fleet don't both `docker run` the one shared container.
var ensureLocks sync.Map // fleetName -> *sync.Mutex

func fleetLock(fleetName string) *sync.Mutex {
	m, _ := ensureLocks.LoadOrStore(fleetName, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// ContainerName is the docker container name for a fleet's shared image cache;
// it doubles as the network DNS name instances' dockerd uses to reach it.
func ContainerName(fleetName string) string {
	return "fleet-" + backend.SanitizeName(fleetName, fleetNameMaxLen) + "-imgcache"
}

// MirrorURL is the registry-mirror URL an instance's dockerd uses to reach the
// fleet's image cache (container name via the fleet network's DNS, plus port).
// It is plain http, so instances must also list MirrorHostPort as an insecure
// registry.
func MirrorURL(fleetName string) string {
	return "http://" + ContainerName(fleetName) + ":" + mirrorPort
}

// MirrorHostPort is the host:port form of the mirror, used for the dockerd
// insecure-registries entry (which takes no scheme).
func MirrorHostPort(fleetName string) string {
	return ContainerName(fleetName) + ":" + mirrorPort
}

// SharedDir returns the validated host .imgcache directory for a fleet. It
// refuses fleet names that would let the path escape the workspaces root.
func SharedDir(fleetName string) (string, error) {
	if fleetName == "" || strings.ContainsAny(fleetName, `/\`) || strings.Contains(fleetName, "..") {
		return "", fmt.Errorf("invalid fleet name %q for image cache dir", fleetName)
	}
	return state.ImageCacheDir(fleetName), nil
}

// dockerRunArgs builds the `docker run` argv for a fleet's image cache server.
// Pure (no side effects) so it can be asserted in tests. The container joins the
// fleet network (so instances reach it by name), runs in pull-through proxy mode
// against Docker Hub, bind-mounts its storage to the host, and uses
// restart=unless-stopped so a docker daemon restart brings the cache back.
func dockerRunArgs(fleetName, network string) []string {
	return []string{
		"run", "-d",
		"--name", ContainerName(fleetName),
		"--network", network,
		"--restart", "unless-stopped",
		"-e", "REGISTRY_PROXY_REMOTEURL=" + upstreamRegistry,
		"-v", state.ImageCacheDir(fleetName) + ":" + containerCachePath,
		image,
	}
}

// EnsureSharedServer makes the fleet's shared registry container exist and run,
// returning the host .imgcache directory. Idempotent and safe to call on every
// instance create/start; runs on the fleet's shared network.
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
		return "", fmt.Errorf("create image cache dir: %w", err)
	}
	network, err := ensureNetwork(fleetName)
	if err != nil {
		return "", fmt.Errorf("ensure fleet network: %w", err)
	}

	name := ContainerName(fleetName)
	running, exists := inspectState(name)
	switch {
	case exists && running:
		flog.Info("image cache server reused", "fleet", fleetName, "container", name)
	case exists && !running:
		if out, err := runDocker("start", name); err != nil {
			return "", fmt.Errorf("start image cache server: %w (%s)", err, out)
		}
		flog.Info("image cache server started", "fleet", fleetName, "container", name)
	default:
		if out, err := runDocker(dockerRunArgs(fleetName, network)...); err != nil {
			if _, exists2 := inspectState(name); !exists2 {
				return "", fmt.Errorf("run image cache server: %w (%s)", err, out)
			}
			flog.Info("image cache server already present", "fleet", fleetName, "container", name)
		} else {
			flog.Info("image cache server created", "fleet", fleetName, "container", name)
		}
	}
	return dir, nil
}

// StopSharedServer force-removes a fleet's image cache container. Best-effort: an
// already-absent container is treated as success.
func StopSharedServer(fleetName string) error {
	name := ContainerName(fleetName)
	out, err := runDocker("rm", "-f", name)
	if err != nil {
		if strings.Contains(out, "No such container") {
			return nil
		}
		return fmt.Errorf("remove image cache server %s: %w (%s)", name, err, out)
	}
	flog.Info("image cache server removed", "fleet", fleetName, "container", name)
	return nil
}

// DeleteCache wipes a fleet's image cache and restarts the server empty. Mirrors
// internal/buildkit.DeleteCache: remove the container, clear the .imgcache
// directory's CONTENTS (keeping the directory), then re-ensure the server.
func DeleteCache(fleetName string) error {
	var wipeErr error
	func() {
		lock := fleetLock(fleetName)
		lock.Lock()
		defer lock.Unlock()
		name := ContainerName(fleetName)
		if out, err := runDocker("rm", "-f", name); err != nil && !strings.Contains(out, "No such container") {
			wipeErr = fmt.Errorf("stop image cache server: %w (%s)", err, out)
			return
		}
		wipeErr = clearSharedDir(fleetName)
	}()

	_, ensureErr := EnsureSharedServer(fleetName)
	if wipeErr != nil {
		return fmt.Errorf("clear image cache: %w", wipeErr)
	}
	if ensureErr != nil {
		return fmt.Errorf("restart image cache server after cache clear: %w", ensureErr)
	}
	flog.Info("image cache cleared", "fleet", fleetName)
	return nil
}

// clearSharedDir wipes the CONTENTS of a fleet's .imgcache directory but keeps
// the directory itself. registry:2 writes storage as its own in-container UID,
// which can leave subdirectories a host-context rm can't traverse; on failure it
// falls back to a throwaway root container that clears the contents.
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
	if out, err := runDocker("run", "--rm", "--user", "0", "--entrypoint", "find", "-v", dir+":/work", image,
		"/work", "-mindepth", "1", "-maxdepth", "1", "-exec", "rm", "-rf", "{}", ";"); err != nil {
		return fmt.Errorf("clear image cache dir %s: %w (%s)", dir, err, out)
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

// ensureDir creates path 0777 (and re-chmods an existing one): bind-mounted into
// containers whose user UID differs from the host's. Lives under ~/.fleet.
func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0777); err != nil {
		return err
	}
	return os.Chmod(path, 0777)
}
