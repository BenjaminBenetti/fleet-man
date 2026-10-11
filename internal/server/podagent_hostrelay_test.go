//go:build !nok8s

package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/BenjaminBenetti/fleet-man/internal/backend/kubernetes"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/hostrelay"
	"github.com/BenjaminBenetti/fleet-man/internal/podwire"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// fakeGateway stands in for the daemon host's listening ports and the
// connections' host_gateway settings.
type fakeGateway struct {
	mu    sync.Mutex
	ports []hostrelay.Port
	scans int
	off   map[string]bool
	errs  []string
}

func (f *fakeGateway) install(g *podGateway) {
	g.scan = func(context.Context) ([]hostrelay.Port, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.scans++
		return append([]hostrelay.Port(nil), f.ports...), nil
	}
	g.loadClusters = func() (*state.KubeClusters, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var reg state.KubeClusters
		for _, id := range []string{"kc-1", "kc-2"} {
			c := fleet.KubeCluster{ID: id}
			if f.off[id] {
				c.HostGateway = fleet.KubeHostGatewayOff
			}
			reg.Clusters = append(reg.Clusters, c)
		}
		return &reg, nil
	}
	g.logError = func(msg string, args ...any) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.errs = append(f.errs, fmt.Sprint(append([]any{msg}, args...)...))
	}
}

func (f *fakeGateway) set(ports ...hostrelay.Port) {
	f.mu.Lock()
	f.ports = ports
	f.mu.Unlock()
}

func (f *fakeGateway) turnOff(id string, off bool) {
	f.mu.Lock()
	if f.off == nil {
		f.off = map[string]bool{}
	}
	f.off[id] = off
	f.mu.Unlock()
	// The settings are re-read once podClustersTTL passes.
}

func (f *fakeGateway) errors() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.errs...)
}

func port(p uint16, dial string) hostrelay.Port {
	return hostrelay.Port{Port: p, Dial: netip.MustParseAddrPort(dial)}
}

// gatewayTarget is podTarget whose devcontainer binds a gpg socket and
// declares host-gateway, serving 3000 itself.
func gatewayTarget(gpgSource string) kubernetes.PodTarget {
	t := podTarget
	t.HostSockets = []hostrelay.Socket{{Target: "/run/itest-host-gpg-agent.sock", Source: gpgSource, Mode: 0o700}}
	t.HostGateway = true
	t.GatewaySkip = []uint16{3000}
	return t
}

// TestPodSocketsCarryTheHostRelays: a pod binds the host-socket targets its
// record lists, and mirrors the daemon host's listening ports when its
// devcontainer declares host-gateway and its connection's host_gateway is
// not off, but for the ports it serves itself; a pod that declares no
// host-gateway mirrors none, and the ports are not even listed for it.
func TestPodSocketsCarryTheHostRelays(t *testing.T) {
	isolateFleetDir(t)
	t.Setenv("FLEET_SSH_AGENT_SOCK", "off")
	h := newService().podAgents
	fg := &fakeGateway{}
	fg.install(h.gateway)
	fg.set(port(22, "127.0.0.1:22"), port(3000, "127.0.0.1:3000"), port(5432, "[::1]:5432"))

	if got := h.sockets(podTarget); got.Host != nil || got.Gateway != nil || fg.scans != 0 {
		t.Fatalf("a pod with no host relays: %+v (scans %d)", got, fg.scans)
	}
	target := gatewayTarget("/home/u/.gnupg/S.gpg-agent.extra")
	got := h.sockets(target)
	if want := []podwire.HostSocket{{Target: "/run/itest-host-gpg-agent.sock", Mode: 0o700}}; !reflect.DeepEqual(got.Host, want) {
		t.Fatalf("host sockets %+v", got.Host)
	}
	if !reflect.DeepEqual(got.Gateway, []uint16{22, 5432}) {
		t.Fatalf("gateway %v", got.Gateway)
	}
	if err := (podwire.DesiredState{Sockets: got}).Validate(); err != nil {
		t.Fatal(err)
	}
	// Listed once for both asks within the interval.
	h.sockets(target)
	if fg.scans != 1 {
		t.Fatalf("%d scans", fg.scans)
	}

	off := target
	off.Cluster = "kc-2"
	fg.turnOff("kc-2", true)
	h.gateway.readAt = time.Time{}
	if got := h.sockets(off); got.Gateway != nil || len(got.Host) != 1 {
		t.Fatalf("host_gateway off: %+v", got)
	}
}

// TestPodGatewayTickFollowsTheHost: while a pod with a session mirrors the
// ports, the hub's tick has the pods told when the listening set changes
// or a connection's host_gateway flips, and only then; once no such pod has
// a session, it lists nothing.
func TestPodGatewayTickFollowsTheHost(t *testing.T) {
	isolateFleetDir(t)
	g := newPodGateway()
	fg := &fakeGateway{}
	fg.install(g)
	fg.set(port(8080, "127.0.0.1:8080"))
	ctx := context.Background()
	live := true
	alive := func(kubernetes.PodTarget) bool { return live }

	if g.tick(ctx, alive) || fg.scans != 0 {
		t.Fatal("a tick with no mirroring pod listed the ports")
	}
	target := gatewayTarget("/s")
	if got := g.portsFor(ctx, target); !reflect.DeepEqual(got, []uint16{8080}) {
		t.Fatalf("ports %v", got)
	}
	if g.tick(ctx, alive) {
		t.Fatal("an unchanged set counted as a change")
	}
	fg.set(port(8080, "127.0.0.1:8080"), port(9000, "192.168.1.5:9000"))
	if !g.tick(ctx, alive) {
		t.Fatal("a new port did not count")
	}
	fg.turnOff("kc-1", true)
	if !g.tick(ctx, alive) {
		t.Fatal("host_gateway turned off did not count")
	}
	if got := g.portsFor(ctx, target); got != nil {
		t.Fatalf("mirrored with host_gateway off: %v", got)
	}
	// A pod without a session is kept a while (it is asked about before its
	// session is live), then forgotten.
	live = false
	fg.set(port(8080, "127.0.0.1:8080"))
	if !g.tick(ctx, alive) {
		t.Fatal("a pod asked about a moment ago was forgotten")
	}
	g.mu.Lock()
	for k, p := range g.pods {
		p.asked = time.Now().Add(-podGatewayForget)
		g.pods[k] = p
	}
	g.mu.Unlock()
	scans := fg.scans
	if g.tick(ctx, alive) || g.tick(ctx, alive) || fg.scans != scans {
		t.Fatal("ticks without a mirroring pod's session listed the ports")
	}
}

// TestPodHostSocketRelay: a connection to a host-socket bind reaches the
// daemon-host socket recorded for its target, half-close included; a target
// fleetd did not record is refused, and so is a recorded one it cannot
// reach, which fleet.log gets at error with its fleet and instance, once
// until it is reached again.
func TestPodHostSocketRelay(t *testing.T) {
	isolateFleetDir(t)
	dir, err := hostrelay.Resolve(shortTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	gpg := filepath.Join(dir, "S.gpg-agent.extra")
	ln, err := net.Listen("unix", gpg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				in, _ := io.ReadAll(c)
				_, _ = c.Write([]byte("signed:" + string(in)))
			}()
		}
	}()
	h := newService().podAgents
	fg := &fakeGateway{}
	fg.install(h.gateway)
	target := gatewayTarget(gpg)
	sess := relayPair(t, func(st *podwire.Stream) { h.inbound(target, st) })

	talk := func() (string, error) {
		conn, err := podwire.DialSock(sess, podwire.TagSockHost, podwire.SockOpen{Target: "/run/itest-host-gpg-agent.sock"})
		if err != nil {
			return "", err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, _ = conn.Write([]byte("payload"))
		halfClose(t, conn)
		out, err := io.ReadAll(conn)
		return string(out), err
	}
	if out, err := talk(); err != nil || out != "signed:payload" {
		t.Fatalf("relayed %q, %v", out, err)
	}
	if _, err := podwire.DialSock(sess, podwire.TagSockHost, podwire.SockOpen{Target: "/var/run/docker.sock"}); !podwire.IsSockRefused(err, podwire.RefusedGate) {
		t.Fatalf("an unrecorded target: %v", err)
	}

	_ = ln.Close()
	for range 3 {
		if _, err := talk(); !podwire.IsSockRefused(err, podwire.RefusedUnreachable) {
			t.Fatalf("an unreachable socket: %v", err)
		}
	}
	errs := fg.errors()
	if len(errs) != 1 || !strings.Contains(errs[0], "alpha") || !strings.Contains(errs[0], "dev-1") || !strings.Contains(errs[0], gpg) {
		t.Fatalf("fleet.log errors %q, want one naming the fleet, instance and socket", errs)
	}
}

// TestPodHostDialRelay: a connection to a mirrored port is dialled on the
// daemon host where the port listens; one to a port outside the mirrored
// set, a port the devcontainer serves itself, a pod that declares no
// host-gateway or whose connection turned it off, is refused, and one the
// daemon host no longer answers is refused as unreachable.
func TestPodHostDialRelay(t *testing.T) {
	isolateFleetDir(t)
	srv, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() {
		for {
			c, err := srv.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				in, _ := io.ReadAll(c)
				_, _ = c.Write([]byte("host:" + string(in)))
			}()
		}
	}()
	p := uint16(srv.Addr().(*net.TCPAddr).Port)
	h := newService().podAgents
	fg := &fakeGateway{}
	fg.install(h.gateway)
	fg.set(port(p, srv.Addr().String()), port(3000, "127.0.0.1:3000"), port(1, "127.0.0.1:1"))
	target := gatewayTarget("/s")
	sess := relayPair(t, func(st *podwire.Stream) { h.inbound(target, st) })

	dial := func(sess *yamux.Session, port uint16) (string, error) {
		conn, err := podwire.DialSock(sess, podwire.TagHostDial, podwire.SockOpen{Port: port})
		if err != nil {
			return "", err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, _ = conn.Write([]byte("GET /token"))
		halfClose(t, conn)
		out, err := io.ReadAll(conn)
		return string(out), err
	}
	if out, err := dial(sess, p); err != nil || out != "host:GET /token" {
		t.Fatalf("relayed %q, %v", out, err)
	}
	for name, port := range map[string]uint16{"not listening": 4999, "the pod's own": 3000} {
		if _, err := dial(sess, port); !podwire.IsSockRefused(err, podwire.RefusedGate) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := dial(sess, 1); !podwire.IsSockRefused(err, podwire.RefusedUnreachable) {
		t.Errorf("a port nothing answers: %v", err)
	}
	plain := relayPair(t, func(st *podwire.Stream) { h.inbound(podTarget, st) })
	if _, err := dial(plain, p); !podwire.IsSockRefused(err, podwire.RefusedGate) {
		t.Errorf("a pod without host-gateway: %v", err)
	}
	fg.turnOff("kc-1", true)
	h.gateway.readAt = time.Time{}
	if _, err := dial(sess, p); !podwire.IsSockRefused(err, podwire.RefusedGate) {
		t.Errorf("host_gateway off: %v", err)
	}
	if errs := fg.errors(); len(errs) != 0 {
		t.Errorf("port refusals logged as errors: %q", errs)
	}
}

// TestPodAgentHubTellsThePodsOfPortChanges: once a pod that mirrors the
// daemon host's ports was asked about, the hub's loop has the pods told when
// the listening set changes, and only then; once that pod has no session,
// nothing more.
func TestPodAgentHubTellsThePodsOfPortChanges(t *testing.T) {
	isolateFleetDir(t)
	t.Setenv("FLEET_SSH_AGENT_SOCK", "off")
	h := newService().podAgents
	fg := &fakeGateway{}
	fg.install(h.gateway)
	fg.set(port(8080, "127.0.0.1:8080"))
	fp := &fakePodPool{}
	h.pool = func() podAgentPool { return fp }
	h.interval = time.Hour
	h.gateway.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "the pool hooked", func() bool {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		return fp.source != nil
	})
	fp.mu.Lock()
	source := fp.source
	fp.mu.Unlock()
	if got := source(gatewayTarget("/s")); !reflect.DeepEqual(got.Gateway, []uint16{8080}) {
		t.Fatalf("gateway %v", got.Gateway)
	}
	steady := func(want int) {
		t.Helper()
		time.Sleep(60 * time.Millisecond)
		if got := fp.count(); got != want {
			t.Fatalf("refreshes %d, want %d", got, want)
		}
	}
	steady(0)
	fg.set(port(8080, "127.0.0.1:8080"), port(9090, "127.0.0.1:9090"))
	waitFor(t, "a refresh for the new port", func() bool { return fp.count() == 1 })
	steady(1)
	h.gateway.mu.Lock()
	for k, p := range h.gateway.pods {
		p.asked = time.Now().Add(-podGatewayForget)
		h.gateway.pods[k] = p
	}
	h.gateway.mu.Unlock()
	fp.mu.Lock()
	fp.dead = true
	fp.mu.Unlock()
	waitFor(t, "the pod without a session forgotten", func() bool {
		h.gateway.mu.Lock()
		defer h.gateway.mu.Unlock()
		return len(h.gateway.pods) == 0
	})
	fg.set()
	steady(1)
}
