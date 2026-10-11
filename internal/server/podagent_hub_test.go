//go:build !nok8s

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/mcpbridge"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// podagent_hub_test.go: the daemon's side of the Kubernetes pods' in-pod
// sockets, with the pod's agent stood in by a yamux session whose streams
// the tests open as the agent does (podwire.DialSock).

var podTarget = kubernetes.PodTarget{Cluster: "kc-1", Fleet: "alpha", Instance: "dev-1", StatefulSet: "fi-alpha-dev-1-abcdefgh"}

// TestInPodSocketNamesAreTheDaemons: the pod binds its sockets under the
// names the daemon's sockets have in a docker instance's control directory,
// which fleet.rc, fleet launch, ssh and the MCP bridge dial.
func TestInPodSocketNamesAreTheDaemons(t *testing.T) {
	for tag, want := range map[podwire.Tag]string{
		podwire.TagSockControl: control.SocketName,
		podwire.TagSockAgent:   agentsock.SocketName,
		podwire.TagSockMCP:     mcpbridge.SocketName,
	} {
		if got := podwire.SockName(tag); got != want {
			t.Errorf("%s: %q, want %q", tag, got, want)
		}
	}
}

// fleetState is a state with fleet alpha (Fleet MCP mcp) holding dev-1.
func fleetState(mcp bool) *state.State {
	return &state.State{Fleets: map[string]*fleet.Fleet{
		"alpha": {Name: "alpha", Settings: fleet.FleetSettings{FleetMCP: mcp}, Instances: []*fleet.Instance{{Name: "dev-1"}}},
	}}
}

// TestPodSocketsFollowTheDaemon: a pod binds the control socket while a TUI
// is attached, the agent socket while the daemon can serve an agent from
// where FLEET_SSH_AGENT_SOCK says (a live override socket, else the relay;
// never with forwarding off), and the MCP socket while its fleet has Fleet
// MCP on (read from the newest state, else from state.json).
func TestPodSocketsFollowTheDaemon(t *testing.T) {
	isolateFleetDir(t)
	svc := newService()
	h := svc.podAgents
	loads := 0
	h.loadState = func() (*state.State, error) { loads++; return fleetState(true), nil }
	var warns []string
	h.warn = func(msg string, _ ...any) { warns = append(warns, msg) }

	t.Setenv(agentsock.EnvOverride, "off")
	if got := h.sockets(podTarget); !reflect.DeepEqual(got, podwire.Sockets{MCP: true}) || loads != 1 {
		t.Fatalf("before any state: %+v (loads %d)", got, loads)
	}
	svc.hub.hasSubs.Store(true)
	h.onState(fleetState(false))
	if got := h.sockets(podTarget); !reflect.DeepEqual(got, podwire.Sockets{Control: true}) || loads != 1 {
		t.Fatalf("a TUI attached, Fleet MCP off: %+v (loads %d)", got, loads)
	}

	dir := shortTempDir(t)
	sock, _ := startFakeAgent(t, dir, "override")
	t.Setenv(agentsock.EnvOverride, sock)
	if got := h.sockets(podTarget); !got.SSHAgent {
		t.Fatalf("a live override: %+v", got)
	}
	t.Setenv(agentsock.EnvOverride, filepath.Join(dir, "vm-only.sock"))
	// Nothing backs the relay in this test: no agent for the pod.
	for range 3 {
		if got := h.sockets(podTarget); got.SSHAgent != agentsock.RelayUsable() {
			t.Fatalf("a missing override: %+v", got)
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], agentsock.EnvOverride) {
		t.Fatalf("warnings %q, want one naming %s", warns, agentsock.EnvOverride)
	}
}

// fakePodPool records what the hub asks of the pool.
type fakePodPool struct {
	mu        sync.Mutex
	source    kubernetes.DesiredSource
	inbound   kubernetes.InboundHandler
	refreshes int
	dead      bool
}

func (p *fakePodPool) SetDesiredSource(fn kubernetes.DesiredSource) {
	p.mu.Lock()
	p.source = fn
	p.mu.Unlock()
}

func (p *fakePodPool) SetInbound(fn kubernetes.InboundHandler) {
	p.mu.Lock()
	p.inbound = fn
	p.mu.Unlock()
}

func (p *fakePodPool) RefreshDesired() {
	p.mu.Lock()
	p.refreshes++
	p.mu.Unlock()
}

// Live: every pod the hub asks about has a session, unless dead says.
func (p *fakePodPool) Live(t kubernetes.PodTarget) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.dead
}

func (p *fakePodPool) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refreshes
}

// TestPodAgentHubTellsThePodsOfChanges: the hub hooks the pool, and has the
// pods' states sent again when a TUI comes or goes or a fleet's Fleet MCP
// flips, and only then.
func TestPodAgentHubTellsThePodsOfChanges(t *testing.T) {
	isolateFleetDir(t)
	t.Setenv(agentsock.EnvOverride, "off")
	svc := newService()
	h := svc.podAgents
	fp := &fakePodPool{}
	h.pool = func() podAgentPool { return fp }
	h.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	waitFor(t, "the pool hooked", func() bool {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		return fp.source != nil && fp.inbound != nil
	})
	steady := func(want int) {
		t.Helper()
		time.Sleep(60 * time.Millisecond)
		if got := fp.count(); got != want {
			t.Fatalf("refreshes %d, want %d", got, want)
		}
	}
	steady(0)
	svc.hub.hasSubs.Store(true)
	waitFor(t, "a refresh for the TUI", func() bool { return fp.count() == 1 })
	steady(1)
	h.onState(fleetState(true))
	waitFor(t, "a refresh for the first state", func() bool { return fp.count() == 2 })
	h.onState(fleetState(true))
	steady(2)
	h.onState(fleetState(false))
	waitFor(t, "a refresh for Fleet MCP off", func() bool { return fp.count() == 3 })
	svc.hub.hasSubs.Store(false)
	waitFor(t, "a refresh for the TUI gone", func() bool { return fp.count() == 4 })
}

// TestPodAgentFallbackWarnsOnlyForPods: a daemon whose FLEET_SSH_AGENT_SOCK
// is a path only a Docker VM has, and which serves no pods, writes no
// warning about pods; the first pod asked about gets the one warning. The
// hub keeps answering throughout: its state listener runs on the hub loop,
// so a hub stuck on its own lock would stall the whole daemon.
func TestPodAgentFallbackWarnsOnlyForPods(t *testing.T) {
	isolateFleetDir(t)
	t.Setenv(agentsock.EnvOverride, filepath.Join(shortTempDir(t), "vm-only.sock"))
	svc := newService()
	h := svc.podAgents
	var mu sync.Mutex
	var warns []string
	h.warn = func(msg string, _ ...any) {
		mu.Lock()
		warns = append(warns, msg)
		mu.Unlock()
	}
	warned := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(warns)
	}
	fp := &fakePodPool{}
	h.pool = func() podAgentPool { return fp }
	h.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the hub did not stop")
		}
	})
	time.Sleep(100 * time.Millisecond)
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		h.onState(fleetState(false))
		if n := warned(); n != 0 {
			t.Errorf("a daemon with no pods warned %d times", n)
		}
		h.sockets(podTarget)
		h.sockets(podTarget)
	}()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("the hub is stuck")
	}
	if n := warned(); n != 1 {
		t.Fatalf("pods asked about: %d warnings, want 1", n)
	}
}

// relayPair is a pod agent's session stood in: streams opened on the
// returned session reach serve as the agent's do.
func relayPair(t *testing.T, serve func(*podwire.Stream)) *yamux.Session {
	t.Helper()
	a, b := net.Pipe()
	agentSess, err := yamux.Server(a, podwire.SessionConfig(podwire.RoleMain))
	if err != nil {
		t.Fatal(err)
	}
	fleetd, err := yamux.Client(b, podwire.SessionConfig(podwire.RoleMain))
	if err != nil {
		t.Fatal(err)
	}
	handlers := map[podwire.Tag]podwire.Handler{}
	for _, tag := range append([]podwire.Tag{podwire.TagSockHost, podwire.TagHostDial, podwire.TagSyncCtl}, podwire.SockTags...) {
		handlers[tag] = serve
	}
	go func() { _ = (&podwire.Mux{Handlers: handlers}).Serve(fleetd) }()
	t.Cleanup(func() {
		_ = agentSess.Close()
		_ = fleetd.Close()
	})
	return agentSess
}

func halfClose(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
}

// TestPodInboundServesEachSocketBehindItsGate: a relayed connection is
// served by the handler of its socket's kind, for the instance the session
// is the pod of, only while that socket's gate is open; a session that names
// no instance, and a kind with no in-pod socket, are refused.
func TestPodInboundServesEachSocketBehindItsGate(t *testing.T) {
	t.Setenv("HOME", shortTempDir(t))
	t.Setenv(agentsock.EnvOverride, "")
	svc := newService()
	opened := make(chan string, 1)
	svc.control = newControlRegistry(func(fleetName, instanceName, url string) {
		opened <- fleetName + "/" + instanceName + " " + url
	}, nil, svc.hub.hasSubscribers)
	h := svc.podAgents
	target := podTarget
	sess := relayPair(t, func(st *podwire.Stream) { h.inbound(target, st) })

	// The control socket: only while a TUI is attached, and its envelopes
	// are the instance's.
	if _, err := podwire.DialSock(sess, podwire.TagSockControl, podwire.SockOpen{}); !podwire.IsSockRefused(err, podwire.RefusedGate) {
		t.Fatalf("control with no TUI: %v", err)
	}
	svc.hub.hasSubs.Store(true)
	conn, err := podwire.DialSock(sess, podwire.TagSockControl, podwire.SockOpen{Peer: &podwire.Peer{UID: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(control.OpenBrowserPayload{URL: "http://localhost:3000"})
	line, _ := json.Marshal(control.Envelope{Type: control.TypeOpenBrowser, Payload: payload})
	_, _ = conn.Write(append(line, '\n'))
	halfClose(t, conn)
	select {
	case got := <-opened:
		if got != "alpha/dev-1 http://localhost:3000" {
			t.Fatalf("browser open %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relayed envelope was not handled")
	}
	_ = conn.Close()

	// The MCP socket: re-checked against state.json for every connection.
	if err := state.Save(fleetState(false)); err != nil {
		t.Fatal(err)
	}
	h.onState(fleetState(true)) // a stale "on" must not let it through
	if _, err := podwire.DialSock(sess, podwire.TagSockMCP, podwire.SockOpen{}); !podwire.IsSockRefused(err, podwire.RefusedGate) {
		t.Fatalf("MCP with Fleet MCP off: %v", err)
	}
	if err := state.Save(fleetState(true)); err != nil {
		t.Fatal(err)
	}
	conn, err = podwire.DialSock(sess, podwire.TagSockMCP, podwire.SockOpen{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "t"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		t.Fatalf("MCP over the relayed connection: %v", err)
	}
	if instr := client.InitializeResult().Instructions; !strings.Contains(instr, `instance "dev-1" of fleet "alpha"`) {
		t.Fatalf("instructions %q", instr)
	}
	_ = client.Close()

	// No instance, no kind.
	empty := relayPair(t, func(st *podwire.Stream) { h.inbound(kubernetes.PodTarget{Cluster: "kc-1"}, st) })
	if _, err := podwire.DialSock(empty, podwire.TagSockControl, podwire.SockOpen{}); !podwire.IsSockRefused(err, podwire.RefusedGate) {
		t.Fatalf("a session with no instance: %v", err)
	}
	if _, err := podwire.DialSock(sess, podwire.TagSyncCtl, podwire.SockOpen{}); !podwire.IsSockRefused(err, podwire.RefusedUnsupported) {
		t.Fatalf("a kind with no in-pod socket: %v", err)
	}
}

// agentListThrough lists the keys an ssh client in the pod sees through a
// relayed agent connection.
func agentListThrough(t *testing.T, sess *yamux.Session) ([]*agent.Key, error) {
	t.Helper()
	conn, err := podwire.DialSock(sess, podwire.TagSockAgent, podwire.SockOpen{})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	return agent.NewClient(conn).List()
}

// TestPodAgentSocketSources: a pod's agent connection goes to the socket
// FLEET_SSH_AGENT_SOCK names when it is live on the daemon host (listing and
// signing only, as the daemon's own agent is served), else through the relay
// (the provider, then the daemon's own agent); with forwarding off it is
// refused.
func TestPodAgentSocketSources(t *testing.T) {
	t.Setenv("HOME", shortTempDir(t))
	svc := newService()
	t.Cleanup(svc.agent.close)
	h := svc.podAgents
	sess := relayPair(t, func(st *podwire.Stream) { h.inbound(podTarget, st) })
	dir := shortTempDir(t)

	t.Setenv(agentsock.EnvOverride, "off")
	if _, err := agentListThrough(t, sess); !podwire.IsSockRefused(err, podwire.RefusedGate) {
		t.Fatalf("forwarding off: %v", err)
	}

	override, overrideKey := startFakeAgent(t, dir, "override")
	t.Setenv(agentsock.EnvOverride, override)
	keys, err := agentListThrough(t, sess)
	if err != nil || len(keys) != 1 || string(keys[0].Marshal()) != string(overrideKey.Marshal()) {
		t.Fatalf("live override: %v %v", keys, err)
	}
	conn, err := podwire.DialSock(sess, podwire.TagSockAgent, podwire.SockOpen{})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := agent.NewClient(conn).RemoveAll(); err == nil {
		t.Fatal("a pod managed the override agent")
	}
	_ = conn.Close()

	// A path only a Docker VM has: the relay, which here is the daemon's own
	// agent.
	daemonSock, daemonKey := startFakeAgent(t, dir, "daemon")
	svc.agent.setFallback(daemonSock)
	t.Setenv(agentsock.EnvOverride, filepath.Join(dir, "vm-only.sock"))
	keys, err = agentListThrough(t, sess)
	if err != nil || len(keys) != 1 || string(keys[0].Marshal()) != string(daemonKey.Marshal()) {
		t.Fatalf("missing override: %v %v", keys, err)
	}
	t.Setenv(agentsock.EnvOverride, "")
	if keys, err = agentListThrough(t, sess); err != nil || len(keys) != 1 || ssh.FingerprintSHA256(keys[0]) != ssh.FingerprintSHA256(daemonKey) {
		t.Fatalf("relay: %v %v", keys, err)
	}
	svc.agent.setFallback("")
	if _, err := agentListThrough(t, sess); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no agent anywhere: %v", err)
	}
}
