package remote

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/gateway"
	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// registry_e2e_test.go runs fleetd's tunnel client against a REAL gateway that
// offers the image registry (a --session-key and an https --public-url, served
// plain as behind a TLS-terminating proxy): image pulls on the public /v2/ route
// reach the Manager's registry listener over TagRegistry streams.

// startRegistryGateway runs a registry-offering gateway on plain loopback
// listeners and returns their addresses (public, grpc).
func startRegistryGateway(t *testing.T) (string, string) {
	t.Helper()
	gw, err := gateway.New(gateway.Config{PublicURL: "https://gw.example.com", SessionKey: "e2e-key"})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	publicLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("public listen: %v", err)
	}
	grpcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grpc listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = gw.ServeListeners(ctx, publicLn, grpcLn) }()
	return publicLn.Addr().String(), grpcLn.Addr().String()
}

// plainRegisterDial opens the gateway's Register stream over h2c, as
// dialGateway does for an http gateway URL.
func plainRegisterDial(grpcAddr string) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, _ string) (net.Conn, error) {
		cc, err := grpc.NewClient("passthrough:///"+grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		stream, err := cc.NewStream(ctx, registerStreamDesc, tunnel.RegisterMethod, grpc.ForceCodec(tunnel.RawCodec{}))
		if err != nil {
			_ = cc.Close()
			return nil, err
		}
		return tunnel.NewStreamConn(stream, func() { _ = cc.Close() }), nil
	}
}

// serveAttempt runs one connectAndServe attempt for d until the test ends and
// returns the CONNECTED status it published.
func serveAttempt(t *testing.T, m *Manager, statusCh <-chan *fleetgrpc.RemoteMcpStatus, d desiredState) *fleetgrpc.RemoteMcpStatus {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.connectAndServe(ctx, d)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return waitForState(t, statusCh, fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED, 5*time.Second)
}

// TestManagerServesRegistryThroughGateway: a registry-only tunnel (remote MCP
// off) negotiates FeatureRegistry, records the gateway's PublicRegistry, and
// serves pulls on /v2/<rid>/... to the registry listener with the rid stripped
// and the credential intact, while MCP requests are refused.
func TestManagerServesRegistryThroughGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	publicAddr, grpcAddr := startRegistryGateway(t)
	mcp := newFakeMCP()
	defer mcp.srv.Close()

	registryLis := NewChanListener()
	defer registryLis.Close()
	registrySrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:test")
		_, _ = io.WriteString(w, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	})}
	go func() { _ = registrySrv.Serve(registryLis) }()
	defer registrySrv.Close()

	statusCh := make(chan *fleetgrpc.RemoteMcpStatus, 64)
	m := NewManager(mcp.port(t), "vtest", func(st *fleetgrpc.RemoteMcpStatus) { statusCh <- st },
		WithDialFunc(plainRegisterDial(grpcAddr)), WithRegistryListener(registryLis))
	const gatewayURL = "https://gw.example.com"
	connected := serveAttempt(t, m, statusCh, desiredState{registry: true, gatewayURL: gatewayURL})

	publicRegistry := loadSession(gatewayURL).PublicRegistry
	rid, ok := strings.CutPrefix(publicRegistry, "gw.example.com/")
	if !ok || len(rid) != 64 {
		t.Fatalf("recorded PublicRegistry = %q, want gw.example.com/<rid>", publicRegistry)
	}
	if strings.Contains(connected.GetPublicUrl(), rid) || strings.Contains(connected.GetPublicWebhookUrl(), rid) {
		t.Fatal("the registry id must differ from the public id")
	}

	req, _ := http.NewRequest(http.MethodGet, "http://"+publicAddr+"/v2/"+rid+"/devcontainer/manifests/sha256:abc", nil)
	req.Header.Set("Authorization", "Basic Zm9vOmJhcg==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "GET /v2/devcontainer/manifests/sha256:abc Basic Zm9vOmJhcg==" {
		t.Fatalf("pull through the gateway -> %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("Docker-Content-Digest") != "sha256:test" {
		t.Fatalf("the registry's headers must pass back, got %q", resp.Header.Get("Docker-Content-Digest"))
	}

	// Remote MCP is off, so its route serves nothing on this tunnel.
	publicID := connected.GetPublicUrl()[strings.LastIndex(connected.GetPublicUrl(), "/")+1:]
	resp, err = http.Get("http://" + publicAddr + "/mcp/" + publicID)
	if err != nil {
		t.Fatalf("mcp: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a registry-only tunnel must not serve MCP")
	}
}

// TestManagerWithoutRegistryListenerGetsNoRegistry: wanting the registry with no
// image server wired requests nothing, so the gateway hands out no
// PublicRegistry and the rid route stays dead.
func TestManagerWithoutRegistryListenerGetsNoRegistry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, grpcAddr := startRegistryGateway(t)
	mcp := newFakeMCP()
	defer mcp.srv.Close()

	statusCh := make(chan *fleetgrpc.RemoteMcpStatus, 64)
	m := NewManager(mcp.port(t), "vtest", func(st *fleetgrpc.RemoteMcpStatus) { statusCh <- st },
		WithDialFunc(plainRegisterDial(grpcAddr)))
	const gatewayURL = "https://gw.example.com"
	serveAttempt(t, m, statusCh, desiredState{mcp: true, registry: true, gatewayURL: gatewayURL})
	if got := loadSession(gatewayURL).PublicRegistry; got != "" {
		t.Fatalf("a daemon with no image server got PublicRegistry %q", got)
	}
}

// TestManagerIgnoresUnrequestedRegistryFeature: a gateway reply that claims the
// registry feature the attempt did not request (registry not wanted) keeps the
// tagged wire it announced, but registry streams reach no listener.
func TestManagerIgnoresUnrequestedRegistryFeature(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cert, pool := genTestTLS(t)
	mcp := newFakeMCP()
	defer mcp.srv.Close()

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	defer ln.Close()
	result := make(chan string, 2)
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
		_ = tunnel.WriteFrame(conn, tunnel.RegisterReply{SessionID: "s", PublicURL: "https://gw/mcp/s", Features: []string{tunnel.FeatureRegistry}})
		sess, err := tunnel.ServerSession(conn, io.Discard)
		if err != nil {
			return
		}
		defer sess.Close()
		// A registry stream: fleetd must close it unanswered.
		rs, err := sess.Open()
		if err != nil {
			return
		}
		_ = tunnel.WriteTag(rs, tunnel.TagRegistry)
		_ = rs.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := rs.Read(make([]byte, 1)); err == nil {
			result <- "registry stream answered"
		} else {
			result <- "registry closed"
		}
		// MCP still works on the tagged wire.
		ms, err := sess.Open()
		if err != nil {
			return
		}
		_ = tunnel.WriteTag(ms, tunnel.TagMCP)
		_, _ = io.WriteString(ms, "GET /echo HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer tagged\r\nConnection: close\r\n\r\n")
		b, _ := io.ReadAll(ms)
		result <- string(b)
		<-gwDone
	}()

	registryLis := NewChanListener()
	defer registryLis.Close()
	go func() {
		if c, err := registryLis.Accept(); err == nil {
			_, _ = io.WriteString(c, "x")
		}
	}()
	m, statusCh := newManagerForTest(mcp.port(t), ln.Addr().String(), pool, WithRegistryListener(registryLis))
	serveAttempt(t, m, statusCh, desiredState{mcp: true, gatewayURL: "https://gw.example.com"})
	if got := <-result; got != "registry closed" {
		t.Fatalf("unrequested registry stream: %s", got)
	}
	if got := <-result; !strings.Contains(got, "Bearer tagged") {
		t.Fatalf("MCP on the tagged wire: %q", got)
	}
}
