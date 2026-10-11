//go:build !nok8s

package server

import (
	"context"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// podagent_hub.go is the daemon's side of the Kubernetes pods' in-pod
// sockets: the DesiredState builder the pool asks which of fleet.sock,
// ssh-agent.sock and mcp.sock each pod binds (the same rules as the host
// sockets of a docker instance: the control socket while a TUI is attached,
// the agent socket while the daemon can serve an agent, the MCP socket while
// the fleet has Fleet MCP on), the push of every change of those answers to
// the pods, and podInbound, which serves each connection a pod's agent relays
// with the same handlers as a host socket's (the control registry's envelope
// handler, the SSH agent relay, the in-instance MCP), after judging its gate
// again: that the pod still had the socket bound is no permission. A relayed
// connection is the instance's whose pod fleetd verified and opened the
// session to (kubernetes.PodTarget), never what the pod says it is.
//
// The control files (the status mod's) reach the pods through the backend
// handle's control channel (controlSinkFor), which the pool delivers with
// the same state.

// podAgentsInterval is how often the answers that do not come with a state
// change (a TUI attached, the agent relay's usability) are checked.
const podAgentsInterval = time.Second

// podAgentPool is the part of the pool the hub drives (*kubernetes.Pool).
type podAgentPool interface {
	SetDesiredSource(kubernetes.DesiredSource)
	SetInbound(kubernetes.InboundHandler)
	RefreshDesired()
}

// podAgentHub is the daemon's side of the pods' in-pod sockets.
type podAgentHub struct {
	svc      *service
	pool     func() podAgentPool
	interval time.Duration
	// loadState reads state.json when no state was handed in yet, and warn
	// is fleet.log's warning (tests stand in for both).
	loadState func() (*state.State, error)
	warn      func(msg string, args ...any)

	wake chan struct{}

	mu sync.Mutex
	// fleetMCP is each fleet's Fleet MCP setting, from the newest state;
	// known once a state was handed in.
	fleetMCP map[string]bool
	known    bool
	// sent is the global answers as the pods last got them.
	sent podGlobals
	// fallbackWarned: the one warning that FLEET_SSH_AGENT_SOCK is no socket
	// on the daemon host was written.
	fallbackWarned bool
}

// podGlobals are the answers that hold for every pod.
type podGlobals struct {
	control, agent bool
}

func newPodAgentHub(svc *service) *podAgentHub {
	return &podAgentHub{
		svc:       svc,
		pool:      func() podAgentPool { return kubernetes.DefaultPool() },
		interval:  podAgentsInterval,
		loadState: state.Load,
		warn:      flog.Warn,
		wake:      make(chan struct{}, 1),
		fleetMCP:  map[string]bool{},
	}
}

// onState hands in a new state (the hub's state listener; it must not
// block): a fleet whose Fleet MCP flipped has its pods told.
func (h *podAgentHub) onState(st *state.State) {
	mcp := make(map[string]bool, len(st.Fleets))
	for name, f := range st.Fleets {
		mcp[name] = f.Settings.FleetMCP
	}
	h.mu.Lock()
	changed := !h.known || !sameFlags(h.fleetMCP, mcp)
	h.fleetMCP, h.known = mcp, true
	h.mu.Unlock()
	if changed {
		h.signal()
	}
}

func sameFlags(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func (h *podAgentHub) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// run gives the pool the hub's answers and handler, then has the pods told
// whenever an answer changes, until ctx ends. Hooking the pool does no I/O:
// a daemon without Kubernetes instances never reaches a cluster.
func (h *podAgentHub) run(ctx context.Context) {
	pool := h.pool()
	pool.SetInbound(h.inbound)
	pool.SetDesiredSource(h.sockets)
	// Never asked with h.mu held: the answers may write the override's
	// warning, which takes it.
	sent := h.globals()
	h.mu.Lock()
	h.sent = sent
	h.mu.Unlock()
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		changed := false
		select {
		case <-ctx.Done():
			return
		case <-h.wake:
			changed = true
		case <-t.C:
		}
		g := h.globals()
		h.mu.Lock()
		if g != h.sent {
			h.sent, changed = g, true
		}
		h.mu.Unlock()
		if changed {
			pool.RefreshDesired()
		}
	}
}

// globals is the answers for every pod now.
func (h *podAgentHub) globals() podGlobals {
	src, _ := agentsock.CurrentPodSource()
	return podGlobals{control: h.svc.hub.hasSubscribers(), agent: agentsock.PodAgentUsable(src)}
}

// sockets is the pool's DesiredSource: which in-pod sockets t's pod binds.
// The pool asks only for a pod it has a session to, so the override's
// fallback warning is written here, once, and never by a daemon that serves
// no pods (whose override may well be a Docker VM's path).
func (h *podAgentHub) sockets(t kubernetes.PodTarget) podwire.Sockets {
	if _, fellBack := agentsock.CurrentPodSource(); fellBack {
		h.warnFallback()
	}
	g := h.globals()
	return podwire.Sockets{Control: g.control, SSHAgent: g.agent, MCP: h.fleetMCPOf(t.Fleet)}
}

// warnFallback says, once, that FLEET_SSH_AGENT_SOCK names no socket on the
// daemon host, so pods reach the agent through the relay instead.
func (h *podAgentHub) warnFallback() {
	h.mu.Lock()
	warned := h.fallbackWarned
	h.fallbackWarned = true
	h.mu.Unlock()
	if !warned {
		h.warn("ssh agent: "+agentsock.EnvOverride+" is not a socket on the daemon host (only a Docker VM has it?); Kubernetes pods use the daemon's agent relay instead",
			"path", agentsock.Override())
	}
}

// fleetMCPOf is fleetName's Fleet MCP setting, from the newest state handed
// in, else read now.
func (h *podAgentHub) fleetMCPOf(fleetName string) bool {
	h.mu.Lock()
	known, on := h.known, h.fleetMCP[fleetName]
	h.mu.Unlock()
	if known {
		return on
	}
	return h.fleetMCPNow(fleetName)
}

// fleetMCPNow reads fleetName's Fleet MCP setting from state.json.
func (h *podAgentHub) fleetMCPNow(fleetName string) bool {
	st, err := h.loadState()
	if err != nil {
		return false
	}
	f := st.Fleets[fleetName]
	return f != nil && f.Settings.FleetMCP
}

// inbound is podInbound: it serves one connection t's pod relayed, by the
// socket it was made to, when that socket's gate is still open.
func (h *podAgentHub) inbound(t kubernetes.PodTarget, st *podwire.Stream) {
	if t.Fleet == "" || t.Instance == "" {
		podwire.RefuseSock(st, podwire.RefusedGate, "the session names no instance")
		return
	}
	key := runtimeKey(t.Fleet, t.Instance)
	refuse := func(why string) {
		flog.Info("pod relay: refused a connection", "fleet", t.Fleet, "instance", t.Instance, "socket", podwire.SockName(st.Tag), "why", why)
		podwire.RefuseSock(st, podwire.RefusedGate, why)
	}
	switch st.Tag {
	case podwire.TagSockControl:
		if !h.svc.hub.hasSubscribers() || h.svc.control == nil {
			refuse("no fleet client is attached to act on it")
			return
		}
		conn, err := podwire.AcceptSock(st)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = control.ServeConn(conn, h.svc.control.handlerFor(t.Fleet, t.Instance))
	case podwire.TagSockAgent:
		if src, _ := agentsock.CurrentPodSource(); src == agentsock.PodAgentOff {
			refuse("SSH agent forwarding is off on the daemon host (" + agentsock.EnvOverride + "=off)")
			return
		}
		conn, err := podwire.AcceptSock(st)
		if err != nil {
			return
		}
		h.svc.agent.serveFromPod(conn, key)
	case podwire.TagSockMCP:
		if !h.fleetMCPNow(t.Fleet) {
			refuse("the fleet's Fleet MCP is off")
			return
		}
		conn, err := podwire.AcceptSock(st)
		if err != nil {
			return
		}
		h.svc.instanceMCP.serveFrom(key)(conn)
	default:
		podwire.RefuseSock(st, podwire.RefusedUnsupported, st.Tag.String()+" streams are not served here")
	}
}
