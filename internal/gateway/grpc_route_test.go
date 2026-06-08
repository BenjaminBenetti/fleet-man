package gateway

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
)

// testChanListener is a tiny in-memory net.Listener used by the simulated fleetd
// to feed demuxed MCP streams to an http.Server.
type testChanListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newTestChanListener() *testChanListener {
	return &testChanListener{conns: make(chan net.Conn), done: make(chan struct{})}
}
func (l *testChanListener) push(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.done:
		_ = c.Close()
	}
}
func (l *testChanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *testChanListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *testChanListener) Addr() net.Addr { return testAddr{} }

type testAddr struct{}

func (testAddr) Network() string { return "test" }
func (testAddr) String() string  { return "test" }

// dialFleetdGRPC simulates a fleet daemon that NEGOTIATES gRPC and demuxes tagged
// streams: TagMCP streams are served by an HTTP echo handler, TagGRPC streams are
// raw byte-echoed (standing in for the native gRPC server). Returns the reply.
func dialFleetdGRPC(t *testing.T, controlAddr string, pool *x509.CertPool) tunnel.RegisterReply {
	t.Helper()
	conn, err := tls.Dial("tcp", controlAddr, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})
	if err != nil {
		t.Fatalf("dial control: %v", err)
	}
	return registerFleetdGRPC(t, conn)
}

// dialFleetdGRPCPlain is dialFleetdGRPC over a PLAINTEXT control connection.
func dialFleetdGRPCPlain(t *testing.T, controlAddr string) tunnel.RegisterReply {
	t.Helper()
	conn, err := net.Dial("tcp", controlAddr)
	if err != nil {
		t.Fatalf("dial control: %v", err)
	}
	return registerFleetdGRPC(t, conn)
}

// registerFleetdGRPC performs the gRPC-negotiating handshake on an already-dialed
// control conn (TLS or plain) and demuxes tagged streams.
func registerFleetdGRPC(t *testing.T, conn net.Conn) tunnel.RegisterReply {
	t.Helper()
	if err := tunnel.WriteFrame(conn, tunnel.RegisterRequest{Features: []string{tunnel.FeatureGRPC}}); err != nil {
		t.Fatalf("write register: %v", err)
	}
	var reply tunnel.RegisterReply
	if err := tunnel.ReadFrame(conn, &reply); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if reply.Error != "" {
		t.Fatalf("gateway refused: %s", reply.Error)
	}
	sess, err := tunnel.ClientSession(conn, io.Discard)
	if err != nil {
		t.Fatalf("client session: %v", err)
	}

	mcpLis := newTestChanListener()
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	})
	mcpSrv := &http.Server{Handler: mux}
	go func() { _ = mcpSrv.Serve(mcpLis) }()

	go func() {
		for {
			stream, err := sess.Accept()
			if err != nil {
				return
			}
			go func(stream net.Conn) {
				tag, err := tunnel.ReadTag(stream)
				if err != nil {
					_ = stream.Close()
					return
				}
				switch tag {
				case tunnel.TagMCP:
					mcpLis.push(stream)
				case tunnel.TagGRPC:
					_, _ = io.Copy(stream, stream) // raw echo (stand-in for gRPC)
				default:
					_ = stream.Close()
				}
			}(stream)
		}
	}()

	t.Cleanup(func() {
		_ = mcpSrv.Close()
		_ = mcpLis.Close()
		_ = sess.Close()
		_ = conn.Close()
	})
	return reply
}

// TestGatewayGRPCRoute drives the new /grpc route end to end (on the gateway
// side): negotiation, the hijack+200 handshake, a raw round-trip through the
// splice, AND that MCP still works on the same negotiated (tagged) session.
func TestGatewayGRPCRoute(t *testing.T) {
	cert, pool := genTestTLS(t)
	s, controlAddr, publicAddr := startTestGateway(t, cert, "https://gw.example.com")

	reply := dialFleetdGRPC(t, controlAddr, pool)
	if !tunnel.HasFeature(reply.Features, tunnel.FeatureGRPC) {
		t.Fatalf("gateway did not negotiate grpc: features=%v", reply.Features)
	}
	id := publicIDOf(t, reply.PublicURL)
	waitRegistered(t, s, id)

	// gRPC route: hijack handshake, then a raw byte round-trip through the splice.
	raw, err := tls.Dial("tcp", publicAddr, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})
	if err != nil {
		t.Fatalf("dial public: %v", err)
	}
	defer raw.Close()
	if _, err := fmt.Fprintf(raw, "GET /grpc/%s HTTP/1.1\r\nHost: gw\r\n\r\n", id); err != nil {
		t.Fatalf("write grpc handshake: %v", err)
	}
	br := bufio.NewReader(raw)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("grpc handshake status = %q err=%v, want 200", status, err)
	}
	for { // consume to end of headers (the blank line)
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if _, err := io.WriteString(raw, "ping-grpc"); err != nil {
		t.Fatalf("write through splice: %v", err)
	}
	got := make([]byte, len("ping-grpc"))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != "ping-grpc" {
		t.Fatalf("grpc splice echo = %q, want ping-grpc", got)
	}

	// MCP still works on the SAME negotiated session (gateway writes TagMCP).
	client := httpsClient(pool)
	if body := getBody(t, client, "https://"+publicAddr+"/mcp/"+id+"/echo", "Bearer mcp-on-grpc"); body != "Bearer mcp-on-grpc" {
		t.Fatalf("mcp on negotiated session = %q", body)
	}
}

// TestGatewayGRPCRoutePlainHTTP drives the /grpc hijack+splice over a fully
// plaintext gateway (no TLS on control or public), proving the splice is
// transport-agnostic and works behind a TLS-terminating reverse proxy.
func TestGatewayGRPCRoutePlainHTTP(t *testing.T) {
	s, controlAddr, publicAddr := startTestGatewayPlain(t, "http://gw.example.com")

	reply := dialFleetdGRPCPlain(t, controlAddr)
	if !tunnel.HasFeature(reply.Features, tunnel.FeatureGRPC) {
		t.Fatalf("gateway did not negotiate grpc: features=%v", reply.Features)
	}
	id := publicIDOf(t, reply.PublicURL)
	waitRegistered(t, s, id)

	raw, err := net.Dial("tcp", publicAddr)
	if err != nil {
		t.Fatalf("dial public: %v", err)
	}
	defer raw.Close()
	if _, err := fmt.Fprintf(raw, "GET /grpc/%s HTTP/1.1\r\nHost: gw\r\n\r\n", id); err != nil {
		t.Fatalf("write grpc handshake: %v", err)
	}
	br := bufio.NewReader(raw)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("grpc handshake status = %q err=%v, want 200", status, err)
	}
	for { // consume to end of headers
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if _, err := io.WriteString(raw, "ping-grpc"); err != nil {
		t.Fatalf("write through splice: %v", err)
	}
	got := make([]byte, len("ping-grpc"))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != "ping-grpc" {
		t.Fatalf("grpc splice echo = %q, want ping-grpc", got)
	}
}

// TestGatewayGRPCNotNegotiated confirms /grpc 404s when the session is a legacy
// (MCP-only) fleetd that did not negotiate the feature.
func TestGatewayGRPCNotNegotiated(t *testing.T) {
	cert, pool := genTestTLS(t)
	s, controlAddr, publicAddr := startTestGateway(t, cert, "https://gw.example.com")

	reply := dialFleetd(t, controlAddr, pool, "") // sends no Features
	if len(reply.Features) != 0 {
		t.Fatalf("legacy fleetd should negotiate no features, got %v", reply.Features)
	}
	id := publicIDOf(t, reply.PublicURL)
	waitRegistered(t, s, id)

	client := httpsClient(pool)
	resp, err := client.Get("https://" + publicAddr + "/grpc/" + id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/grpc on a non-negotiated session -> %d, want 404", resp.StatusCode)
	}
}

func TestGatewayGRPCUnknownSession404(t *testing.T) {
	cert, pool := genTestTLS(t)
	_, _, publicAddr := startTestGateway(t, cert, "https://gw.example.com")
	client := httpsClient(pool)
	resp, err := client.Get("https://" + publicAddr + "/grpc/" + strings.Repeat("b", 64))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/grpc unknown -> %d, want 404", resp.StatusCode)
	}
}
