package remote

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/gateway"
	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
)

// registry_test.go covers the registry prefix the Manager reports and the
// dedicated registry connection: opened on demand, used for pulls, closed
// when idle, retried when the gateway refuses or drops it.

// countingConn counts the bytes written to a connection.
type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countingConn) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return c.Conn.Write(p)
}

// registryServer serves a registry listener with a handler that answers
// every request with a fixed body.
func registryServer(t *testing.T) *ChanListener {
	t.Helper()
	lis := NewChanListener()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("layer-bytes ", 1000))
	})}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = lis.Close()
	})
	return lis
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (m *Manager) registrySnapshot() registryState {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	return m.reg
}

// pullThrough pulls a blob through the gateway's public route. The gateway
// binds a session just after its register reply, so a first pull may meet a
// 404 for a moment.
func pullThrough(t *testing.T, publicAddr, rid string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodGet, "http://"+publicAddr+"/v2/"+rid+"/devcontainer/blobs/sha256:abc", nil)
		req.SetBasicAuth("kc", "cred")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("pull: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), "layer-bytes") {
			t.Fatalf("pull -> %d %q", resp.StatusCode, body[:min(len(body), 80)])
		}
		return
	}
}

func TestRegistryPrefixAndReasons(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mcp := newFakeMCP()
	defer mcp.srv.Close()
	_, grpcAddr := startRegistryGateway(t)
	const gatewayURL = "https://gw.example.com"

	newM := func(opts ...Option) (*Manager, chan *fleetgrpc.RemoteMcpStatus) {
		statusCh := make(chan *fleetgrpc.RemoteMcpStatus, 64)
		return NewManager(mcp.port(t), "vtest", func(st *fleetgrpc.RemoteMcpStatus) { statusCh <- st },
			append([]Option{WithDialFunc(plainRegisterDial(grpcAddr))}, opts...)...), statusCh
	}

	m, ch := newM()
	st := serveAttempt(t, m, ch, desiredState{mcp: true, registry: true, gatewayURL: gatewayURL})
	if p, why := m.Registry(); p != "" || why != whyRegistryNotWired {
		t.Fatalf("no listener: %q %q", p, why)
	}
	if st.GetRegistryError() != whyRegistryNotWired || st.GetPublicRegistry() != "" {
		t.Fatalf("no listener: status %v", st)
	}

	m, ch = newM(WithRegistryListener(registryServer(t)))
	st = serveAttempt(t, m, ch, desiredState{mcp: true, gatewayURL: gatewayURL})
	if p, why := m.Registry(); p != "" || why != whyRegistryOff {
		t.Fatalf("not wanted: %q %q", p, why)
	}
	if st.GetRegistryError() != "" || st.GetPublicRegistry() != "" {
		t.Fatalf("a status while the registry is off names it: %v", st)
	}

	var mu sync.Mutex
	var emitted []string
	type told struct{ prefix, why string }
	var states []told
	m, ch = newM(WithRegistryListener(registryServer(t)), WithRegistryChange(func(p string) {
		mu.Lock()
		defer mu.Unlock()
		emitted = append(emitted, p)
	}), WithRegistryState(func(p, why string) {
		mu.Lock()
		defer mu.Unlock()
		states = append(states, told{p, why})
	}))
	st = serveAttempt(t, m, ch, desiredState{mcp: true, registry: true, gatewayURL: gatewayURL})
	p, why := m.Registry()
	if st.GetPublicRegistry() != p || st.GetRegistryError() != "" {
		t.Fatalf("connected status %v, want public registry %q", st, p)
	}
	mu.Lock()
	if len(states) != 2 || states[0] != (told{"", whyRegistryPending}) || states[1] != (told{p, ""}) {
		t.Fatalf("registry states told %+v", states)
	}
	mu.Unlock()
	if !strings.HasPrefix(p, "gw.example.com/") || len(p) != len("gw.example.com/")+64 || why != "" {
		t.Fatalf("negotiated: %q %q", p, why)
	}
	if got := loadSession(gatewayURL).PublicRegistry; got != p {
		t.Fatalf("session file has %q, the Manager %q", got, p)
	}
	mu.Lock()
	if len(emitted) != 2 || emitted[0] != "" || emitted[1] != p {
		t.Fatalf("emitted %q", emitted)
	}
	mu.Unlock()
	// A reconnect to the same gateway keeps the prefix while it connects.
	m.registryConnecting(desiredState{mcp: true, registry: true, gatewayURL: gatewayURL})
	if again, _ := m.Registry(); again != p {
		t.Fatalf("reconnecting: %q", again)
	}
	// A fresh daemon starts from the session file.
	fresh, _ := newM(WithRegistryListener(registryServer(t)))
	fresh.registryConnecting(desiredState{mcp: true, registry: true, gatewayURL: gatewayURL})
	if got, _ := fresh.Registry(); got != p {
		t.Fatalf("fresh daemon: %q, want the session file's %q", got, p)
	}
	fresh.registryConnecting(desiredState{mcp: true, registry: true, gatewayURL: "https://other.example.com"})
	if got, why := fresh.Registry(); got != "" || why != whyRegistryPending {
		t.Fatalf("another gateway: %q %q", got, why)
	}
}

func TestRegistryNotOfferedByTheGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mcp := newFakeMCP()
	defer mcp.srv.Close()
	// No --session-key: the gateway does not offer the registry.
	gw, err := gateway.New(gateway.Config{PublicURL: "https://gw.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	publicLn, _ := net.Listen("tcp", "127.0.0.1:0")
	grpcLn, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = gw.ServeListeners(ctx, publicLn, grpcLn) }()

	statusCh := make(chan *fleetgrpc.RemoteMcpStatus, 64)
	m := NewManager(mcp.port(t), "vtest", func(st *fleetgrpc.RemoteMcpStatus) { statusCh <- st },
		WithDialFunc(plainRegisterDial(grpcLn.Addr().String())), WithRegistryListener(registryServer(t)))
	st := serveAttempt(t, m, statusCh, desiredState{mcp: true, registry: true, gatewayURL: "https://gw.example.com"})
	want := whyRegistryNotOffered + "no --session-key"
	if p, why := m.Registry(); p != "" || why != want {
		t.Fatalf("a gateway without the feature: %q %q, want %q", p, why, want)
	}
	if st.GetRegistryError() != want {
		t.Fatalf("the connected status names %q, want %q", st.GetRegistryError(), want)
	}
	m.WarmRegistry()
	time.Sleep(200 * time.Millisecond)
	if st := m.registrySnapshot(); st.attaches != 0 {
		t.Fatal("attached to a gateway that does not offer the registry")
	}
}

func TestRegistryConnectionOnDemand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mcp := newFakeMCP()
	defer mcp.srv.Close()
	publicAddr, grpcAddr := startRegistryGateway(t)
	var attachBytes atomic.Int64
	var wraps atomic.Int32
	statusCh := make(chan *fleetgrpc.RemoteMcpStatus, 64)
	m := NewManager(mcp.port(t), "vtest", func(st *fleetgrpc.RemoteMcpStatus) { statusCh <- st },
		WithDialFunc(plainRegisterDial(grpcAddr)), WithRegistryListener(registryServer(t)),
		WithRegistryConnWrapper(func(c net.Conn) net.Conn {
			wraps.Add(1)
			return countingConn{Conn: c, n: &attachBytes}
		}))
	m.registryIdle = 500 * time.Millisecond
	serveAttempt(t, m, statusCh, desiredState{registry: true, gatewayURL: "https://gw.example.com"})
	prefix, _ := m.Registry()
	rid := strings.TrimPrefix(prefix, "gw.example.com/")

	// Nothing asked for it: no registry connection.
	time.Sleep(200 * time.Millisecond)
	if st := m.registrySnapshot(); st.attaches != 0 || wraps.Load() != 0 {
		t.Fatalf("attached unasked: %+v", st)
	}

	// A pull on the main tunnel (one fleetd did not start) opens it.
	pullThrough(t, publicAddr, rid)
	waitUntil(t, "the registry connection", func() bool { return m.registrySnapshot().attached })
	if wraps.Load() != 1 {
		t.Fatalf("the registry connection was wrapped %d times", wraps.Load())
	}
	// Later pulls go over it.
	before := attachBytes.Load()
	pullThrough(t, publicAddr, rid)
	if attachBytes.Load()-before < 12000 {
		t.Fatalf("the pull sent %d bytes over the registry connection", attachBytes.Load()-before)
	}

	// Idle: closed, and the next pull goes down the main tunnel again.
	waitUntil(t, "the idle close", func() bool { return !m.registrySnapshot().attached })
	pullThrough(t, publicAddr, rid)

	// WarmRegistry opens it ahead of a pull, and keeps it while asked.
	waitUntil(t, "the idle close", func() bool { return !m.registrySnapshot().attached })
	attaches := m.registrySnapshot().attaches
	m.WarmRegistry()
	waitUntil(t, "a warmed registry connection", func() bool { st := m.registrySnapshot(); return st.attached && st.attaches == attaches+1 })
}

// fakeAttachGateway is a TLS gateway stand-in that registers the main tunnel
// with the registry feature and refuses (or drops) the first attaches.
type fakeAttachGateway struct {
	ln       net.Listener
	refuse   int // attaches to refuse with an error
	drop     int // attaches to accept and close at once
	mu       sync.Mutex
	attaches []tunnel.RegisterRequest
	served   atomic.Int32
}

func (g *fakeAttachGateway) serve(t *testing.T) {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			var req tunnel.RegisterRequest
			if err := tunnel.ReadFrame(conn, &req); err != nil {
				_ = conn.Close()
				return
			}
			if req.Attach == "" {
				_ = tunnel.WriteFrame(conn, tunnel.RegisterReply{SessionID: "secret-1", PublicURL: "https://gw/mcp/p", PublicRegistry: "gw.example.com/" + strings.Repeat("d", 64), Features: []string{tunnel.FeatureRegistry}})
				sess, err := tunnel.ServerSession(conn, io.Discard)
				if err != nil {
					return
				}
				<-sess.CloseChan()
				return
			}
			g.mu.Lock()
			g.attaches = append(g.attaches, req)
			n := len(g.attaches)
			g.mu.Unlock()
			if n <= g.refuse {
				_ = tunnel.WriteFrame(conn, tunnel.RegisterReply{Error: "gateway: no registry session for this attach"})
				_ = conn.Close()
				return
			}
			_ = tunnel.WriteFrame(conn, tunnel.RegisterReply{Features: []string{tunnel.FeatureRegistry}})
			if n <= g.refuse+g.drop {
				_ = conn.Close()
				return
			}
			sess, err := tunnel.RegistryServerSession(conn, io.Discard)
			if err != nil {
				return
			}
			g.served.Add(1)
			<-sess.CloseChan()
		}()
	}
}

func TestRegistryAttachRetriesRefusalsAndDrops(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cert, pool := genTestTLS(t)
	mcp := newFakeMCP()
	defer mcp.srv.Close()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	gw := &fakeAttachGateway{ln: ln, refuse: 2, drop: 1}
	go gw.serve(t)

	m, statusCh := newManagerForTest(mcp.port(t), ln.Addr().String(), pool, WithRegistryListener(registryServer(t)))
	m.WarmRegistry()
	serveAttempt(t, m, statusCh, desiredState{mcp: true, registry: true, gatewayURL: "https://gw.example.com"})
	waitUntil(t, "a registry connection after 2 refusals and a drop", func() bool { return gw.served.Load() == 1 })
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.attaches) != 4 {
		t.Fatalf("%d attach attempts, want 4", len(gw.attaches))
	}
	for _, req := range gw.attaches {
		if req.Attach != tunnel.AttachRegistry || req.SessionID != "secret-1" || !tunnel.HasFeature(req.Features, tunnel.FeatureRegistry) {
			t.Fatalf("attach request %+v", req)
		}
	}
}

func TestReconcileCarriesTheRegistryFlag(t *testing.T) {
	m := NewManager(1, "vtest", func(*fleetgrpc.RemoteMcpStatus) {})
	m.Reconcile(true, false, false, true, "https://gw.example.com")
	if d := m.desired; !d.registry || !d.mcp {
		t.Fatalf("desired %+v", d)
	}
	m.Reconcile(true, false, false, false, "https://gw.example.com")
	if d := m.desired; d.registry || !d.mcp {
		t.Fatalf("desired %+v", d)
	}
	// An unchanged value wakes nobody.
	select {
	case <-m.wake:
	default:
	}
	m.Reconcile(true, false, false, false, "https://gw.example.com")
	select {
	case <-m.wake:
		t.Fatal("an unchanged Reconcile woke the supervisor")
	default:
	}
}

// The image registry alone brings the tunnel up (TunnelDesired includes
// it), and asks for nothing but FeatureRegistry.
func TestRegistryAloneWantsTheTunnel(t *testing.T) {
	d := desiredState{registry: true, gatewayURL: "https://gw.example.com"}
	if !d.on() {
		t.Fatal("Enable Image Registry alone must bring the tunnel up")
	}
	if (desiredState{registry: true}).on() {
		t.Fatal("no tunnel without a gateway URL")
	}
	m := NewManager(1, "vtest", func(*fleetgrpc.RemoteMcpStatus) {}, WithRegistryListener(NewChanListener()))
	if got := m.features(d); len(got) != 1 || got[0] != tunnel.FeatureRegistry {
		t.Fatalf("features %v, want only the registry", got)
	}
}

// The reasons for having no registry name what to change: a gateway too old
// to know the feature (it gives no reason of its own), one configured
// without it (its reason), no gateway, or the toggle.
func TestRegistryReasonsNameTheCause(t *testing.T) {
	m := NewManager(1, "vtest", func(*fleetgrpc.RemoteMcpStatus) {}, WithRegistryListener(NewChanListener()))
	on := desiredState{registry: true, gatewayURL: "https://gw.example.com"}
	for _, tc := range []struct {
		name string
		set  func()
		want string
	}{
		{"old gateway", func() { m.registryNotNegotiated(on, "") }, whyRegistryTooOld},
		{"no session key", func() { m.registryNotNegotiated(on, "no --session-key") }, whyRegistryNotOffered + "no --session-key"},
		{"path prefix", func() { m.registryNotNegotiated(on, "--public-url has a path prefix") }, whyRegistryNotOffered + "--public-url has a path prefix"},
		{"toggle off", func() {
			m.registryNotNegotiated(desiredState{mcp: true, gatewayURL: on.gatewayURL}, "no --session-key")
		}, whyRegistryOff},
		{"tunnel off: toggle", func() { m.registryTunnelOff(desiredState{gatewayURL: on.gatewayURL}) }, whyRegistryOff},
		{"tunnel off: no gateway", func() { m.registryTunnelOff(desiredState{registry: true}) }, whyRegistryNoGateway},
	} {
		tc.set()
		if p, why := m.Registry(); p != "" || why != tc.want {
			t.Errorf("%s: %q %q, want %q", tc.name, p, why, tc.want)
		}
	}
	unwired := NewManager(1, "vtest", func(*fleetgrpc.RemoteMcpStatus) {})
	unwired.registryTunnelOff(desiredState{registry: true})
	if _, why := unwired.Registry(); why != whyRegistryNotWired {
		t.Errorf("no listener: %q", why)
	}
}
