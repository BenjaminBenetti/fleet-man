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
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"math/bits"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
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

// A fleet network takes a /24 of subnetBase rather than docker's choice.
// docker gives a new network the first free /16 of its default pools
// (172.18.0.0/16 on a host with only its default bridge), and a
// docker-in-docker instance's own dockerd, up before the instance joins the
// network, gave its docker0 that same /16 (the first one its default bridge
// does not cover): `docker network connect` then refuses the instance
// ("conflicts with existing route"), and neither cache reaches it. No docker
// engine hands out 172.16.0.0/16 itself (its pools start at 172.17.0.0/16),
// so a nested engine's networks never meet a fleet network there.
var subnetBase = netip.MustParsePrefix("172.16.0.0/16")

const subnetBits = 24

// procNetRoute is the Linux routing table hostNetworks reads.
const procNetRoute = "/proc/net/route"

// hostNetworks are the networks this host already reaches, which a fleet
// network must not take over: its interfaces' and, on Linux, its routes' (a
// VPN's routed ranges hold none of the host's addresses). docker checks a
// subnet it is given against its own networks only. A package var so tests
// can stand in for the host.
var hostNetworks = func() []netip.Prefix {
	var nets []netip.Prefix
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if p, err := netip.ParsePrefix(ipn.String()); err == nil {
					nets = append(nets, p.Masked())
				}
			}
		}
	}
	if table, err := os.ReadFile(procNetRoute); err == nil {
		nets = append(nets, routedNetworks(table)...)
	}
	return nets
}

// routedNetworks are the destinations of a /proc/net/route table but its
// default route.
func routedNetworks(table []byte) []netip.Prefix {
	var nets []netip.Prefix
	sc := bufio.NewScanner(bytes.NewReader(table))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 {
			continue
		}
		dst, err1 := strconv.ParseUint(f[1], 16, 32)
		mask, err2 := strconv.ParseUint(f[7], 16, 32)
		if err1 != nil || err2 != nil || mask == 0 {
			continue // the header, or the default route
		}
		var ip [4]byte
		binary.LittleEndian.PutUint32(ip[:], uint32(dst))
		nets = append(nets, netip.PrefixFrom(netip.AddrFrom4(ip), bits.OnesCount32(uint32(mask))).Masked())
	}
	return nets
}

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
// present network is reused, an absent one is created (on a subnet of
// subnetBase). Serialized per fleet so concurrent creates don't race on
// `docker network create`.
func EnsureNetwork(fleetName string) (string, error) {
	lock := fleetLock(fleetName)
	lock.Lock()
	defer lock.Unlock()

	name := NetworkName(fleetName)
	if networkExists(name) {
		return name, nil
	}
	if subnet, out, err := createNetwork(name); err != nil {
		// A concurrent (cross-process) or manual create may have won the name
		// race; if the network exists now, treat it as success.
		if !networkExists(name) {
			return "", fmt.Errorf("create fleet network %s: %w (%s)", name, err, out)
		}
		flog.Info("fleet network already present", "fleet", fleetName, "network", name)
	} else {
		flog.Info("fleet network created", "fleet", fleetName, "network", name, "subnet", subnet)
	}
	return name, nil
}

// createNetwork creates the network name on the first /24 of subnetBase that
// no host network overlaps and docker has not given another network, or, with
// none left, wherever docker chooses. subnet is the one it asked for ("" for
// docker's choice).
func createNetwork(name string) (subnet, out string, err error) {
	taken := hostNetworks()
	for s := subnetBase.Addr(); subnetBase.Contains(s); s = nextSubnet(s) {
		p := netip.PrefixFrom(s, subnetBits)
		if overlapsAny(p, taken) {
			continue
		}
		out, err = runDocker("network", "create", "--subnet", p.String(), name)
		// docker: "invalid pool request: Pool overlaps with other one on
		// this address space" (another network has it).
		if err == nil || !strings.Contains(out, "overlaps") {
			return p.String(), out, err
		}
	}
	out, err = runDocker("network", "create", name)
	return "", out, err
}

// nextSubnet is the first address of the /subnetBits after the one at s.
func nextSubnet(s netip.Addr) netip.Addr {
	b := s.As4()
	v := binary.BigEndian.Uint32(b[:]) + 1<<(32-subnetBits)
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

func overlapsAny(p netip.Prefix, nets []netip.Prefix) bool {
	for _, n := range nets {
		if p.Overlaps(n) {
			return true
		}
	}
	return false
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
