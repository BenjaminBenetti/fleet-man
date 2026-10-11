package podspec

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/hostrelay"
	"github.com/BenjaminBenetti/fleet-man/internal/podinit"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
)

// host_relays.go decides the two docker features a pod reaches the daemon
// host through fleet's relays for:
//
//   - Socket binds. A bind of the user's ssh-agent socket becomes a link at
//     its target to fleet's agent relay in the control directory (pod-init
//     makes it; the relay's gates are the daemon's, as for any instance).
//     Every other daemon-host socket bind, and each socket in a bound socket
//     directory, is recorded (PodSpec.HostSockets): the pod's agent binds a
//     socket at the target, and fleetd relays each connection to the
//     daemon-host path recorded here, never one the pod names.
//   - host-gateway. host.docker.internal (and each name the run gives
//     host-gateway) resolves to HostGatewayIP in the pod, where the agent
//     mirrors the daemon host's listening TCP ports, leaving out the ports
//     the devcontainer serves itself (GatewaySkip).

// finishHostRelays records the host-socket binds, links the ssh-agent binds
// and settles host-gateway, once the mounts and ports are final.
func (t *translation) finishHostRelays() {
	// A bind at one of a bound socket directory's sockets is that socket, as
	// the deeper mount is on docker (the targets a pod's agent binds are
	// distinct, or the pod gets no state at all).
	bound := map[string]bool{}
	for _, m := range t.spec.Mounts {
		if m.Kind == MountSSHAgent || m.Kind == MountHostSocket {
			bound[m.Target] = true
		}
	}
	for _, m := range t.spec.Mounts {
		switch m.Kind {
		case MountSSHAgent:
			t.spec.PID1.Links = append(t.spec.PID1.Links, podinit.Link{Path: m.Target, Target: agentsock.ContainerSocketPath})
		case MountHostSocket:
			t.addHostSocket(m.Target, m.Source)
		case MountHostSocketDir:
			entries, err := t.host.ReadDir(m.Source)
			if err != nil {
				t.refuse(&Refusal{Code: CodeHostMount, Reason: fmt.Sprintf("the socket directory %s bound at %s: %v", m.Source, m.Target, err)})
				continue
			}
			for _, e := range entries {
				target := path.Join(m.Target, e.Name())
				if e.Type()&fs.ModeSocket != 0 && !bound[target] {
					t.addHostSocket(target, filepath.Join(m.Source, e.Name()))
				}
			}
		}
	}
	if n := len(t.spec.HostSockets); n > podwire.MaxHostSockets {
		t.refuse(&Refusal{Code: CodeUnsupportedMount, Reason: fmt.Sprintf("the devcontainer binds %d daemon-host sockets; fleet relays at most %d into a pod", n, podwire.MaxHostSockets)})
	}
	t.finishHostGateway()
}

// addHostSocket records the daemon-host socket source bound at target, with
// the socket's permission bits.
func (t *translation) addHostSocket(target, source string) {
	if !podwire.ValidHostSocketTarget(target) {
		t.refuse(&Refusal{Code: CodeUnsupportedMount, Reason: fmt.Sprintf("the socket bind at %s: a unix socket's path holds at most %d bytes", target, podwire.MaxSocketPath)})
		return
	}
	if dir := podwire.ReservedHostSocketDir(target); dir != "" {
		// The pod's agent would not bind it.
		t.refuse(&Refusal{Code: CodeUnsupportedMount, Reason: fmt.Sprintf("the socket bind at %s: fleet relays no daemon-host socket into %s in a pod", target, dir)})
		return
	}
	var perm fs.FileMode
	if info, err := t.host.Stat(source); err == nil {
		perm = info.Mode().Perm()
	}
	t.spec.HostSockets = append(t.spec.HostSockets, hostrelay.Socket{Target: target, Source: source, Mode: perm})
}

// finishHostGateway keeps the mirror off a pod on the node's network (its
// listeners would open the daemon host's ports to every process on the
// node), names what the mirror does differently from docker, and leaves the
// devcontainer's own ports to the pod.
func (t *translation) finishHostGateway() {
	if !t.spec.HostGateway {
		return
	}
	if t.spec.HostNetwork {
		t.spec.HostGateway = false
		t.warn("host-gateway: the pod is on the node's network (--network host), where fleet does not mirror the daemon host's ports (every process on the node would reach them); %s resolves to %s, which there is the node's own loopback", hostDockerInternal, HostGatewayIP)
		return
	}
	t.spec.GatewaySkip = t.ownPorts()
	t.warn("host-gateway: %s is %s in the pod, where fleet mirrors every TCP port the daemon host listens on, loopback-only ones too; a pod process cannot listen on 0.0.0.0 at a mirrored port (the devcontainer's forwardPorts and published ports are not mirrored)", hostDockerInternal, HostGatewayIP)
}

// ownPorts are the ports the devcontainer serves in the pod: its published
// container ports (-p, appPort) and its forwardPorts on this container
// (a number, or localhost:<port>), sorted.
func (t *translation) ownPorts() []uint16 {
	set := map[uint16]bool{}
	for _, p := range t.spec.Ports {
		if p.ContainerPort > 0 && p.ContainerPort <= 65535 {
			set[uint16(p.ContainerPort)] = true
		}
	}
	for _, raw := range t.merged.ForwardPorts {
		if p, ok := forwardPort(raw); ok {
			set[p] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]uint16, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// forwardPort reads one forwardPorts entry that names a port of this
// container: 3000, or "localhost:3000" (another host's, "db:5432", is a
// compose service's).
func forwardPort(raw json.RawMessage) (uint16, bool) {
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		p, err := strconv.ParseUint(n.String(), 10, 16)
		return uint16(p), err == nil && p > 0
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0, false
	}
	host, port, ok := strings.Cut(s, ":")
	if !ok {
		port = host
	} else if host != "localhost" && host != "127.0.0.1" {
		return 0, false
	}
	p, err := strconv.ParseUint(port, 10, 16)
	return uint16(p), err == nil && p > 0
}
