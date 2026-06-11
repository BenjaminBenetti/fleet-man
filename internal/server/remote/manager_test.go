package remote

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
)

// --- unit tests ------------------------------------------------------------

func TestGRPCTarget(t *testing.T) {
	cases := []struct {
		url, wantTarget string
		wantTLS         bool
		wantErr         bool
	}{
		{url: "https://gw.example.com", wantTarget: "dns:///gw.example.com:443", wantTLS: true},
		{url: "https://gw.example.com:50051", wantTarget: "dns:///gw.example.com:50051", wantTLS: true},
		// http is accepted (plaintext h2c, e.g. behind a TLS-terminating proxy):
		// target defaults to :80 and no TLS.
		{url: "http://gw.example.com", wantTarget: "dns:///gw.example.com:80", wantTLS: false},
		{url: "http://gw.example.com:8080", wantTarget: "dns:///gw.example.com:8080", wantTLS: false},
		{url: "ftp://gw.example.com", wantErr: true}, // only http/https
		{url: "https://", wantErr: true},             // no host
		{url: "://bad", wantErr: true},               // unparseable
	}
	for _, c := range cases {
		target, useTLS, err := grpcTarget(c.url)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got target=%q", c.url, target)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.url, err)
			continue
		}
		if target != c.wantTarget || useTLS != c.wantTLS {
			t.Errorf("%q: got (%q,%v), want (%q,%v)", c.url, target, useTLS, c.wantTarget, c.wantTLS)
		}
	}
}

// TestFeaturesGatedOnFleetEnabled pins the "Enable Remote Fleet" gate: the grpc
// tunnel feature is requested ONLY when a gRPC listener is wired AND the user
// enabled remote fleet. Not requesting it is what keeps the gateway's gRPC
// endpoint dead for this daemon — the gateway refuses to route fleet-session
// RPCs for a session that did not negotiate grpc.
func TestFeaturesGatedOnFleetEnabled(t *testing.T) {
	discard := func(*fleetgrpc.RemoteMcpStatus) {}
	withLis := NewManager(1, "v", discard, WithGRPCListener(NewChanListener()))
	noLis := NewManager(1, "v", discard)

	if got := withLis.features(desiredState{mcp: true, grpc: true}); !tunnel.HasFeature(got, tunnel.FeatureGRPC) {
		t.Fatalf("fleet enabled + listener wired should request grpc, got %v", got)
	}
	if got := withLis.features(desiredState{mcp: true, grpc: false}); len(got) != 0 {
		t.Fatalf("fleet disabled must request no features, got %v", got)
	}
	if got := noLis.features(desiredState{mcp: true, grpc: true}); len(got) != 0 {
		t.Fatalf("no gRPC listener must request no features, got %v", got)
	}
}

func TestNextBackoffCaps(t *testing.T) {
	d := initialBackoff
	for i := 0; i < 20; i++ {
		d = nextBackoff(d)
	}
	if d != maxBackoff {
		t.Fatalf("backoff should saturate at %v, got %v", maxBackoff, d)
	}
}

func TestJitterBounds(t *testing.T) {
	for i := 0; i < 1000; i++ {
		j := jitter(time.Second)
		if j < 0 || j > time.Second {
			t.Fatalf("jitter out of [0,1s]: %v", j)
		}
	}
	if jitter(0) != 0 {
		t.Fatal("jitter(0) must be 0")
	}
}

func TestSessionFileRoundTripAndStale(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := fleetpaths.EnsureDir(); err != nil {
		t.Fatalf("ensure dir: %v", err)
	}

	in := sessionFile{SessionID: "sid1", SessionToken: "jwt1", PublicURL: "https://gw/mcp/sid1", GatewayURL: "https://gw"}
	if err := saveSession(in); err != nil {
		t.Fatalf("saveSession: %v", err)
	}
	// Same gateway URL -> returned.
	if got := loadSession("https://gw"); got != in {
		t.Fatalf("loadSession same gw: got %+v want %+v", got, in)
	}
	// Different gateway URL -> stale, ignored (zero value).
	if got := loadSession("https://other"); got != (sessionFile{}) {
		t.Fatalf("loadSession different gw should be zero, got %+v", got)
	}
	// Missing file -> zero value.
	_ = os.Remove(filepath.Join(fleetpaths.Dir(), "gateway_session.json"))
	if got := loadSession("https://gw"); got != (sessionFile{}) {
		t.Fatalf("loadSession missing should be zero, got %+v", got)
	}
}

// --- end-to-end over a real in-test TLS gateway ---------------------------

// genTestTLS returns a self-signed cert valid for 127.0.0.1 plus a pool that
// trusts it (the leaf is its own CA).
func genTestTLS(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fleet-test-gateway"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("x509 keypair: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append cert to pool")
	}
	return cert, pool
}

func waitForState(t *testing.T, ch <-chan *fleetgrpc.RemoteMcpStatus, want fleetgrpc.RemoteMcpConn, timeout time.Duration) *fleetgrpc.RemoteMcpStatus {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case st := <-ch:
			if st.GetState() == want {
				return st
			}
		case <-deadline:
			t.Fatalf("timed out waiting for state %v", want)
			return nil
		}
	}
}

// newManagerForTest builds a Manager whose dial reaches addr over TLS trusting
// pool, with status pushed to the returned channel.
func newManagerForTest(mcpPort int, addr string, pool *x509.CertPool) (*Manager, <-chan *fleetgrpc.RemoteMcpStatus) {
	statusCh := make(chan *fleetgrpc.RemoteMcpStatus, 64)
	m := NewManager(mcpPort, "vtest", func(st *fleetgrpc.RemoteMcpStatus) { statusCh <- st })
	m.dial = func(ctx context.Context, _ string) (net.Conn, error) {
		d := &tls.Dialer{Config: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}}
		return d.DialContext(ctx, "tcp", addr)
	}
	return m, statusCh
}

// TestManagerConnectsServesAndDisables drives the full lifecycle against a real
// TLS gateway: CONNECTING -> CONNECTED (with the gateway's public URL), the
// session is persisted, the tunnel actually serves a request, and disabling
// tears it down to UNSPECIFIED.
func TestManagerConnectsServesAndDisables(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cert, pool := genTestTLS(t)
	mcp := newFakeMCP()
	defer mcp.srv.Close()

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	defer ln.Close()

	served := make(chan string, 1) // the auth header the tunnel delivered to the MCP server
	gwDone := make(chan struct{})
	defer close(gwDone)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req tunnel.RegisterRequest
		if err := tunnel.ReadFrame(conn, &req); err != nil {
			return
		}
		_ = tunnel.WriteFrame(conn, tunnel.RegisterReply{SessionID: "sess-A", SessionToken: "tok-A", PublicURL: "https://gw/mcp/sess-A", PublicGRPCURL: "https://gw:50051/grpc/sess-A"})
		sess, err := tunnel.ServerSession(conn, io.Discard)
		if err != nil {
			return
		}
		defer sess.Close()
		// Prove the tunnel serves: issue a request back down it to the loopback MCP.
		hc := gatewayHTTPClient(sess)
		hreq, _ := http.NewRequest(http.MethodGet, "http://tunnel/echo", nil)
		hreq.Header.Set("Authorization", "Bearer e2e")
		if resp, err := hc.Do(hreq); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			served <- string(b)
		} else {
			served <- "ERR:" + err.Error()
		}
		<-gwDone
	}()

	m, statusCh := newManagerForTest(mcp.port(t), ln.Addr().String(), pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	m.Reconcile(true, false, "https://gw.example.com")

	connected := waitForState(t, statusCh, fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED, 5*time.Second)
	if connected.GetPublicUrl() != "https://gw/mcp/sess-A" {
		t.Fatalf("public url = %q, want https://gw/mcp/sess-A", connected.GetPublicUrl())
	}
	// The gateway-assigned Public GRPC URL rides the same status.
	if connected.GetPublicGrpcUrl() != "https://gw:50051/grpc/sess-A" {
		t.Fatalf("public grpc url = %q, want https://gw:50051/grpc/sess-A", connected.GetPublicGrpcUrl())
	}

	// The tunnel actually carried a request, auth header intact.
	select {
	case got := <-served:
		if got != "Bearer e2e" {
			t.Fatalf("tunnel served auth = %q, want %q", got, "Bearer e2e")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gateway never served a request over the tunnel")
	}

	// Sticky session (id + resume token + URLs) persisted for reconnect.
	if s := loadSession("https://gw.example.com"); s.SessionID != "sess-A" || s.SessionToken != "tok-A" ||
		s.PublicURL != "https://gw/mcp/sess-A" || s.PublicGRPCURL != "https://gw:50051/grpc/sess-A" {
		t.Fatalf("session not persisted: %+v", s)
	}

	// Disable -> tears down to UNSPECIFIED.
	m.Reconcile(false, false, "https://gw.example.com")
	waitForState(t, statusCh, fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_UNSPECIFIED, 5*time.Second)
}

// TestManagerStickyReconnect verifies that after an established connection drops,
// the manager reconnects supplying the previously-assigned session id so the
// gateway can hand back the SAME public URL.
func TestManagerStickyReconnect(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cert, pool := genTestTLS(t)
	mcp := newFakeMCP()
	defer mcp.srv.Close()

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	defer ln.Close()

	regs := make(chan tunnel.RegisterRequest, 8)
	var conns atomic.Int32
	gwDone := make(chan struct{})
	defer close(gwDone)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var req tunnel.RegisterRequest
				if err := tunnel.ReadFrame(conn, &req); err != nil {
					return
				}
				n := conns.Add(1)
				regs <- req
				sid := req.SessionID
				if sid == "" {
					sid = "sticky-1"
				}
				if err := tunnel.WriteFrame(conn, tunnel.RegisterReply{SessionID: sid, SessionToken: "tok-sticky", PublicURL: "https://gw/mcp/" + sid}); err != nil {
					return
				}
				if n == 1 {
					return // drop immediately to force a sticky reconnect
				}
				sess, err := tunnel.ServerSession(conn, io.Discard)
				if err != nil {
					return
				}
				defer sess.Close()
				<-gwDone
			}(conn)
		}
	}()

	m, statusCh := newManagerForTest(mcp.port(t), ln.Addr().String(), pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	m.Reconcile(true, false, "https://gw.example.com")

	// First registration: no prior session id or resume token.
	if got := waitForReg(t, regs, 5*time.Second); got.SessionID != "" || got.SessionToken != "" {
		t.Fatalf("first registration carried id=%q token=%q, want both empty", got.SessionID, got.SessionToken)
	}
	// Second registration (after the forced drop): the sticky id AND the cached
	// session token from the first reply.
	if got := waitForReg(t, regs, 5*time.Second); got.SessionID != "sticky-1" || got.SessionToken != "tok-sticky" {
		t.Fatalf("reconnect registration carried id=%q token=%q, want sticky-1/tok-sticky", got.SessionID, got.SessionToken)
	}
	// And it lands CONNECTED on the kept connection.
	waitForState(t, statusCh, fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED, 5*time.Second)
}

func waitForReg(t *testing.T, ch <-chan tunnel.RegisterRequest, timeout time.Duration) tunnel.RegisterRequest {
	t.Helper()
	select {
	case req := <-ch:
		return req
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a registration")
		return tunnel.RegisterRequest{}
	}
}

// TestManagerReconcileIdempotentKeepsTunnel pins the no-op Reconcile: SetConfig
// calls Reconcile on EVERY config save, and a save that does not change the
// remote settings must NOT bounce an established tunnel — for a remote client
// that tunnel carries the SetConfig RPC's own reply (issue #141). After the
// manager is CONNECTED, a Reconcile with identical values (modulo URL
// whitespace, which Reconcile trims) must produce no re-registration at the
// gateway and no status transition away from CONNECTED.
func TestManagerReconcileIdempotentKeepsTunnel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cert, pool := genTestTLS(t)
	mcp := newFakeMCP()
	defer mcp.srv.Close()

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	defer ln.Close()

	regs := make(chan tunnel.RegisterRequest, 8)
	gwDone := make(chan struct{})
	defer close(gwDone)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var req tunnel.RegisterRequest
				if err := tunnel.ReadFrame(conn, &req); err != nil {
					return
				}
				regs <- req
				if err := tunnel.WriteFrame(conn, tunnel.RegisterReply{SessionID: "s", PublicURL: "https://gw/mcp/s"}); err != nil {
					return
				}
				sess, err := tunnel.ServerSession(conn, io.Discard)
				if err != nil {
					return
				}
				defer sess.Close()
				<-gwDone // hold the session open until the test ends
			}(conn)
		}
	}()

	m, statusCh := newManagerForTest(mcp.port(t), ln.Addr().String(), pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	m.Reconcile(true, false, "https://gw.example.com")
	waitForReg(t, regs, 5*time.Second)
	waitForState(t, statusCh, fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED, 5*time.Second)

	// Same desired state — what SetConfig does for the common "saved an
	// unrelated setting" case. The padded URL pins that the comparison happens
	// AFTER TrimSpace, matching what Reconcile stores.
	m.Reconcile(true, false, "  https://gw.example.com  ")

	// The established tunnel must stay up: no new registration and no status
	// transition away from CONNECTED within the observation window.
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case req := <-regs:
			t.Fatalf("idempotent Reconcile bounced the tunnel: new registration %+v", req)
		case st := <-statusCh:
			if st.GetState() != fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED {
				t.Fatalf("idempotent Reconcile changed status to %v", st.GetState())
			}
		case <-deadline:
			return
		}
	}
}

// TestManagerReconcileDisableConverges hammers Reconcile with interleaved
// enable/disable toggles (exercising the desired/attemptCancel race the code
// review flagged) and asserts the manager settles on UNSPECIFIED — i.e. a
// disable is never left running a stale tunnel.
func TestManagerReconcileDisableConverges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cert, pool := genTestTLS(t)
	mcp := newFakeMCP()
	defer mcp.srv.Close()

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	defer ln.Close()

	gwDone := make(chan struct{})
	defer close(gwDone)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var req tunnel.RegisterRequest
				if err := tunnel.ReadFrame(conn, &req); err != nil {
					return
				}
				if err := tunnel.WriteFrame(conn, tunnel.RegisterReply{SessionID: "s", PublicURL: "https://gw/mcp/s"}); err != nil {
					return
				}
				sess, err := tunnel.ServerSession(conn, io.Discard)
				if err != nil {
					return
				}
				defer sess.Close()
				<-gwDone
			}(conn)
		}
	}()

	m, statusCh := newManagerForTest(mcp.port(t), ln.Addr().String(), pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	for i := 0; i < 30; i++ {
		m.Reconcile(true, false, "https://gw.example.com")
		m.Reconcile(false, false, "https://gw.example.com")
	}

	// After the toggles settle, the LAST state observed must be UNSPECIFIED —
	// the manager must not be left connected/connecting with the feature off.
	last := drainUntilIdle(statusCh, 400*time.Millisecond, 5*time.Second)
	if last != fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_UNSPECIFIED {
		t.Fatalf("after disable toggles, settled on %v, want UNSPECIFIED", last)
	}
}

// drainUntilIdle reads statuses until none arrive for `idle` (or `total`
// elapses), returning the last state seen.
func drainUntilIdle(ch <-chan *fleetgrpc.RemoteMcpStatus, idle, total time.Duration) fleetgrpc.RemoteMcpConn {
	last := fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_UNSPECIFIED
	idleTimer := time.NewTimer(idle)
	defer idleTimer.Stop()
	deadline := time.After(total)
	for {
		select {
		case st := <-ch:
			last = st.GetState()
			if !idleTimer.Stop() {
				<-idleTimer.C
			}
			idleTimer.Reset(idle)
		case <-idleTimer.C:
			return last
		case <-deadline:
			return last
		}
	}
}

// TestManagerErrorsWhenMcpDown reports an error (not a crash) when enabled but
// the local MCP server isn't running (mcpPort == 0).
func TestManagerErrorsWhenMcpDown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	statusCh := make(chan *fleetgrpc.RemoteMcpStatus, 64)
	m := NewManager(0, "vtest", func(st *fleetgrpc.RemoteMcpStatus) { statusCh <- st })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	m.Reconcile(true, false, "https://gw.example.com")

	st := waitForState(t, statusCh, fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_ERROR, 5*time.Second)
	if st.GetError() == "" {
		t.Fatal("error status should carry a message")
	}
}
