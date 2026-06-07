// Package fleetnet manages a per-fleet user-defined docker network that lets a
// fleet's instances reach its shared per-fleet cache servers — the apt-cacher-ng
// deb cache (internal/debcache) and the registry pull-through image cache
// (internal/imagecache) — by container NAME.
//
// Buildkit (internal/buildkit) shares its server through a bind-mounted unix
// socket and so needs no networking. These two caches are HTTP services, so an
// instance must be able to open a TCP connection to them. The DEFAULT docker
// bridge provides no DNS, and host-published ports would collide across fleets,
// so a dedicated user-defined network is the robust choice: every container on
// it resolves the others by name (stable across restarts — unlike IPs — and
// with no host-port conflicts).
//
// Like internal/buildkit this is HOST-side orchestration of the `docker` CLI and
// is deliberately server-side. Every operation is idempotent and best-effort:
// callers treat errors as warnings, never as a reason to abort provisioning.
package fleetnet

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// execCommand builds the host command. Package var so tests can stub the docker
// binary itself when exercising the real runDocker path.
var execCommand = exec.Command

// fleetNameMaxLen caps the sanitized fleet segment of the network name (matches
// internal/buildkit's container-name cap).
const fleetNameMaxLen = 40

// runDocker runs `docker <args...>` and returns its combined output (trimmed)
// and error. Package var so tests can stub the docker interaction without a
// daemon, mirroring internal/buildkit.runDocker.
var runDocker = func(args ...string) (string, error) {
	out, err := backend.NewCmd(execCommand("docker", args...), nil).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ensureLocks serializes EnsureNetwork per fleet so concurrent instance creates
// in the same fleet don't both `docker network create` and have the loser fail
// on a name conflict. Keyed per fleet so one fleet never blocks another.
var ensureLocks sync.Map // fleetName -> *sync.Mutex

func fleetLock(fleetName string) *sync.Mutex {
	m, _ := ensureLocks.LoadOrStore(fleetName, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// NetworkName is the docker network name for a fleet's shared cache network.
// Sanitized so arbitrary fleet names yield a valid, collision-resistant name;
// the fleet/-net affixes namespace it away from other docker networks.
func NetworkName(fleetName string) string {
	return "fleet-" + backend.SanitizeName(fleetName, fleetNameMaxLen) + "-net"
}

// EnsureNetwork makes the fleet's shared cache network exist, returning its
// name. Idempotent and safe to call on every cache ensure / instance connect: a
// present network is reused, an absent one is created. Serialized per fleet so
// concurrent creates don't race on `docker network create`.
func EnsureNetwork(fleetName string) (string, error) {
	lock := fleetLock(fleetName)
	lock.Lock()
	defer lock.Unlock()

	name := NetworkName(fleetName)
	if networkExists(name) {
		return name, nil
	}
	if out, err := runDocker("network", "create", name); err != nil {
		// A concurrent (cross-process) or manual create may have won the name
		// race; if the network exists now, treat it as success.
		if !networkExists(name) {
			return "", fmt.Errorf("create fleet network %s: %w (%s)", name, err, out)
		}
		flog.Info("fleet network already present", "fleet", fleetName, "network", name)
	} else {
		flog.Info("fleet network created", "fleet", fleetName, "network", name)
	}
	return name, nil
}

// ConnectInstance attaches a container to the fleet's shared cache network
// (ensuring the network exists first) so it can reach the cache servers by name.
// Idempotent: an already-connected container is treated as success. A blank
// containerID is a no-op.
func ConnectInstance(fleetName, containerID string) error {
	if containerID == "" {
		return nil
	}
	name, err := EnsureNetwork(fleetName)
	if err != nil {
		return err
	}
	if out, err := runDocker("network", "connect", name, containerID); err != nil {
		// docker reports an already-attached endpoint as
		// "...endpoint with name <c> already exists in network <n>".
		if strings.Contains(out, "already exists") || strings.Contains(out, "already connected") {
			return nil
		}
		return fmt.Errorf("connect %s to fleet network %s: %w (%s)", containerID, name, err, out)
	}
	flog.Info("instance joined fleet network", "fleet", fleetName, "network", name, "container", containerID)
	return nil
}

// RemoveNetwork deletes the fleet's shared cache network. Best-effort: an
// already-absent network is success (idempotent teardown). It is only removable
// once every container has been disconnected, so callers must remove the cache
// containers (and tear down the instances) first.
func RemoveNetwork(fleetName string) error {
	name := NetworkName(fleetName)
	if out, err := runDocker("network", "rm", name); err != nil {
		if strings.Contains(out, "No such network") || strings.Contains(out, "not found") {
			return nil
		}
		return fmt.Errorf("remove fleet network %s: %w (%s)", name, err, out)
	}
	flog.Info("fleet network removed", "fleet", fleetName, "network", name)
	return nil
}

// networkExists reports whether a docker network with the given name exists.
// `docker network inspect` exits non-zero for a missing network, which we map to
// false (mirroring internal/buildkit.inspectState).
func networkExists(name string) bool {
	_, err := runDocker("network", "inspect", "-f", "{{.Id}}", name)
	return err == nil
}
