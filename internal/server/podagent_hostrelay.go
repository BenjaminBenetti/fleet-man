//go:build !nok8s

package server

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/hostrelay"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// podagent_hostrelay.go is the daemon's side of the pods' host relays, the
// two docker features a pod reaches the daemon host through:
//
//   - Host-socket binds (TagSockHost). A pod binds the targets its
//     instance record lists (InstanceRecord.HostSockets, carried by the
//     verified session's PodTarget), and each connection it relays names
//     one; fleetd dials the daemon-host path it recorded for that target at
//     create (hostrelay.Dial, which follows no link that appeared since),
//     never a path the pod says. A target fleetd did not record is refused;
//     a recorded socket it cannot reach is logged at error, with the fleet
//     and instance, once until it is reached again.
//   - host-gateway (TagHostDial). A pod whose devcontainer declares it, on a
//     connection whose host_gateway is not off, mirrors the daemon host's
//     listening TCP ports (hostrelay.Scanner, every podGatewayInterval while
//     such a pod has a session) but for the ones it serves itself; each
//     connection it relays names a port, which fleetd dials where the daemon
//     host listens (127.0.0.1 for one on every interface) only while it is
//     in that set.

// podGatewayInterval is how often the daemon host's listening ports are
// listed while a pod mirrors them.
const podGatewayInterval = 5 * time.Second

// podClustersTTL is how long the connections' host_gateway settings are
// kept before kube_clusters.json is read again.
const podClustersTTL = 2 * time.Second

// podGatewayForget is how long a mirroring pod without a session is kept:
// one is asked about while its session is being made, before it is live.
const podGatewayForget = 30 * time.Second

// gatewayPod is a pod that mirrors the ports, and when it was last asked
// about.
type gatewayPod struct {
	target kubernetes.PodTarget
	asked  time.Time
}

// podGateway is the hub's host relays: the listening ports of the daemon
// host, which connections mirror them, and the failures logged.
type podGateway struct {
	// scan lists the daemon host's listening ports, loadClusters the
	// connections, and logError is fleet.log's error (tests stand in for
	// all three).
	scan         func(context.Context) ([]hostrelay.Port, error)
	loadClusters func() (*state.KubeClusters, error)
	logError     func(msg string, args ...any)
	interval     time.Duration

	mu        sync.Mutex
	ports     []hostrelay.Port
	scannedAt time.Time
	scanErr   string
	// unsent: a listing found a set other than the last one, which the
	// pods have not been told of yet (a listing made for one pod's state or
	// one connection tells only that pod, or none).
	unsent bool
	// pods are the pods that mirror the ports, as last asked about, by
	// connection and StatefulSet.
	pods map[string]gatewayPod
	// mirrors is each connection's host_gateway verdict, read at readAt.
	mirrors map[string]bool
	readAt  time.Time
	// failed is the host-socket dial failure last logged, by instance and
	// target.
	failed map[string]string
}

func newPodGateway() *podGateway {
	scanner := &hostrelay.Scanner{}
	return &podGateway{
		scan:         scanner.Scan,
		loadClusters: state.LoadKubeClusters,
		logError:     flog.Error,
		interval:     podGatewayInterval,
		pods:         map[string]gatewayPod{},
		failed:       map[string]string{},
	}
}

// hostSockets is what t's pod binds for its host-socket binds.
func hostSockets(t kubernetes.PodTarget) []podwire.HostSocket {
	if len(t.HostSockets) == 0 {
		return nil
	}
	out := make([]podwire.HostSocket, 0, len(t.HostSockets))
	for _, s := range t.HostSockets {
		out = append(out, podwire.HostSocket{Target: s.Target, Mode: s.Mode.Perm()})
	}
	return out
}

// recordedSource is the daemon-host path fleetd recorded for t's
// host-socket bind at target.
func recordedSource(t kubernetes.PodTarget, target string) (string, bool) {
	for _, s := range t.HostSockets {
		if s.Target == target && target != "" {
			return s.Source, true
		}
	}
	return "", false
}

// portsFor is the ports t's pod mirrors now: none unless its devcontainer
// declares host-gateway and its connection allows it, else the daemon
// host's listening ports but the ones it serves itself, at most
// podwire.MaxGatewayPorts (the lowest).
func (g *podGateway) portsFor(ctx context.Context, t kubernetes.PodTarget) []uint16 {
	if !t.HostGateway {
		return nil
	}
	g.mu.Lock()
	g.pods[t.Cluster+"/"+t.StatefulSet] = gatewayPod{target: t, asked: time.Now()}
	g.mu.Unlock()
	if !g.mirrorsOn(t.Cluster) {
		return nil
	}
	var out []uint16
	for _, p := range g.current(ctx) {
		if slices.Contains(t.GatewaySkip, p.Port) {
			continue
		}
		out = append(out, p.Port)
		if len(out) == podwire.MaxGatewayPorts {
			break
		}
	}
	return out
}

// dialAddr is where fleetd dials t's pod's connection to port, or why it
// does not.
func (g *podGateway) dialAddr(ctx context.Context, t kubernetes.PodTarget, port uint16) (netip.AddrPort, string) {
	switch {
	case !t.HostGateway:
		return netip.AddrPort{}, "the devcontainer declares no host-gateway"
	case !g.mirrorsOn(t.Cluster):
		return netip.AddrPort{}, "host_gateway is off on the instance's connection"
	case slices.Contains(t.GatewaySkip, port):
		return netip.AddrPort{}, "the devcontainer serves port " + strconv.Itoa(int(port)) + " itself"
	}
	for _, p := range g.current(ctx) {
		if p.Port == port {
			return p.Dial, ""
		}
	}
	return netip.AddrPort{}, "the daemon host does not listen at port " + strconv.Itoa(int(port))
}

// mirrorsOn reports whether connection id's host_gateway lets its pods
// mirror the daemon host's ports (a connection fleetd cannot read does not).
func (g *podGateway) mirrorsOn(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mirrors == nil || time.Since(g.readAt) >= podClustersTTL {
		g.readClustersLocked()
	}
	return g.mirrors[id]
}

func (g *podGateway) readClustersLocked() {
	g.readAt = time.Now()
	reg, err := g.loadClusters()
	if err != nil {
		return
	}
	g.mirrors = make(map[string]bool, len(reg.Clusters))
	for i := range reg.Clusters {
		g.mirrors[reg.Clusters[i].ID] = reg.Clusters[i].MirrorsHostGateway()
	}
}

// current is the daemon host's listening ports, listed within the last two
// intervals, else now.
func (g *podGateway) current(ctx context.Context) []hostrelay.Port {
	g.mu.Lock()
	fresh := !g.scannedAt.IsZero() && time.Since(g.scannedAt) < 2*g.interval
	ports := g.ports
	g.mu.Unlock()
	if fresh {
		return ports
	}
	return g.rescan(ctx)
}

// rescan lists the daemon host's listening ports, noting a set other than
// the last one as unsent. A failure is logged once, until it changes.
func (g *podGateway) rescan(ctx context.Context) []hostrelay.Port {
	ports, err := g.scan(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		if g.scanErr != err.Error() {
			g.scanErr = err.Error()
			flog.Warn("host-gateway: cannot list the daemon host's listening ports; pods mirror none", "err", err)
		}
		ports = nil
	} else {
		g.scanErr = ""
	}
	// The first listing changes nothing a pod was told: none was told of an
	// earlier one.
	if !g.scannedAt.IsZero() && !slices.Equal(ports, g.ports) {
		g.unsent = true
	}
	g.ports, g.scannedAt = ports, time.Now()
	return ports
}

// tick is the hub's periodic check while pods mirror the ports: it forgets
// the pods that have had no session (live) since podGatewayForget after they
// were last asked about, and reports whether what the others mirror changed
// (the ports, or a connection's host_gateway) since the last tick that said
// so, whichever listing found it.
func (g *podGateway) tick(ctx context.Context, live func(kubernetes.PodTarget) bool) bool {
	g.mu.Lock()
	pods := make([]gatewayPod, 0, len(g.pods))
	for _, p := range g.pods {
		pods = append(pods, p)
	}
	g.mu.Unlock()
	var mirroring []kubernetes.PodTarget
	for _, p := range pods {
		t := p.target
		if live(t) || time.Since(p.asked) < podGatewayForget {
			mirroring = append(mirroring, t)
			continue
		}
		g.mu.Lock()
		if cur, ok := g.pods[t.Cluster+"/"+t.StatefulSet]; ok && !cur.asked.After(p.asked) {
			delete(g.pods, t.Cluster+"/"+t.StatefulSet)
		}
		g.mu.Unlock()
	}
	if len(mirroring) == 0 {
		return false
	}
	g.mu.Lock()
	before := make(map[string]bool, len(mirroring))
	for _, t := range mirroring {
		before[t.Cluster] = g.mirrors[t.Cluster]
	}
	g.readClustersLocked()
	turned := false
	for id, was := range before {
		if g.mirrors[id] != was {
			turned = true
		}
	}
	g.mu.Unlock()
	g.rescan(ctx)
	g.mu.Lock()
	changed := g.unsent
	g.unsent = false
	g.mu.Unlock()
	return changed || turned
}

// noteDial records how a dial of t's host socket at target went: a failure
// is logged at error, with the fleet and instance, unless the same one was
// already; a success forgets it.
func (g *podGateway) noteDial(t kubernetes.PodTarget, target, source string, err error) {
	key := t.Fleet + "/" + t.Instance + " " + target
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil {
		delete(g.failed, key)
		return
	}
	if g.failed[key] == err.Error() {
		return
	}
	g.failed[key] = err.Error()
	g.logError("pod relay: could not reach a host socket the devcontainer binds", "fleet", t.Fleet, "instance", t.Instance, "target", target, "source", source, "err", err)
}

// serveHostSocket serves one connection t's pod made to a host-socket bind:
// relayed to the daemon-host path recorded for its target, when there is
// one.
func (h *podAgentHub) serveHostSocket(t kubernetes.PodTarget, st *podwire.Stream) {
	var open podwire.SockOpen
	if err := podwire.DecodeHeader(st.Header, &open); err != nil {
		st.Penalize()
		podwire.RefuseSock(st, podwire.RefusedGate, err.Error())
		return
	}
	source, ok := recordedSource(t, open.Target)
	if !ok {
		// The agent binds only the targets fleetd sent it: one that names
		// another broke the protocol (and is slowed, as its warnings are).
		st.Penalize()
		flog.Warn("pod relay: refused a host socket fleet did not record for the instance", "fleet", t.Fleet, "instance", t.Instance, "target", open.Target)
		podwire.RefuseSock(st, podwire.RefusedGate, "fleet recorded no host socket at "+strconv.Quote(open.Target)+" for the instance")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostrelay.DialTimeout)
	up, err := hostrelay.Dial(ctx, source)
	cancel()
	h.gateway.noteDial(t, open.Target, source, err)
	if err != nil {
		podwire.RefuseSock(st, podwire.RefusedUnreachable, err.Error())
		return
	}
	conn, err := podwire.AcceptSock(st)
	if err != nil {
		_ = up.Close()
		return
	}
	hostrelay.Splice(conn, up)
}

// serveHostDial serves one connection t's pod made to a mirrored port:
// dialled on the daemon host where it listens, while the pod's gates let it.
func (h *podAgentHub) serveHostDial(t kubernetes.PodTarget, st *podwire.Stream) {
	var open podwire.SockOpen
	if err := podwire.DecodeHeader(st.Header, &open); err != nil {
		st.Penalize()
		podwire.RefuseSock(st, podwire.RefusedGate, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostrelay.DialTimeout)
	defer cancel()
	addr, why := h.gateway.dialAddr(ctx, t, open.Port)
	if why != "" {
		flog.Info("pod relay: refused a host-gateway connection", "fleet", t.Fleet, "instance", t.Instance, "port", open.Port, "why", why)
		podwire.RefuseSock(st, podwire.RefusedGate, why)
		return
	}
	var d net.Dialer
	up, err := d.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		flog.Info("pod relay: could not reach a daemon-host port", "fleet", t.Fleet, "instance", t.Instance, "addr", addr.String(), "err", err)
		podwire.RefuseSock(st, podwire.RefusedUnreachable, err.Error())
		return
	}
	conn, err := podwire.AcceptSock(st)
	if err != nil {
		_ = up.Close()
		return
	}
	hostrelay.Splice(conn, up)
}
