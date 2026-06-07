package gateway

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
)

// genTestTLS returns a self-signed cert valid for 127.0.0.1 + a pool trusting it.
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
	keyDER, _ := x509.MarshalECPrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append cert")
	}
	return cert, pool
}

// startTestGateway builds a Server with the given cert and starts it on ephemeral
// control + public TLS listeners, returning their addresses.
func startTestGateway(t *testing.T, cert tls.Certificate, publicBase string) (*Server, string, string) {
	t.Helper()
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	s := &Server{
		cfg:       Config{PublicURL: publicBase, MaxSessions: 64},
		reg:       newRegistry(publicBase, 64),
		tlsConfig: tlsCfg,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	controlLn, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("control listen: %v", err)
	}
	publicLn, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("public listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.serve(ctx, controlLn, publicLn) }()
	return s, controlLn.Addr().String(), publicLn.Addr().String()
}

// fleetdMCPHandler is fleetd's stand-in MCP server: it serves a few routes so the
// test can assert routing, auth passthrough, and SSE streaming.
func fleetdMCPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "root") })
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	})
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		n := 0
		fmt.Sscanf(r.URL.Query().Get("n"), "%d", &n)
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < n; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			fl.Flush()
		}
	})
	return mux
}

// dialFleetd simulates a fleet daemon: it dials the control endpoint, performs
// the tunnel handshake, and serves fleetdMCPHandler over the resulting yamux
// session (exactly as fleetd's serveProxy does, but with a test handler). It
// returns the gateway's reply. cleanup is registered on t.
func dialFleetd(t *testing.T, controlAddr string, pool *x509.CertPool, sessionID string) tunnel.RegisterReply {
	t.Helper()
	conn, err := tls.Dial("tcp", controlAddr, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})
	if err != nil {
		t.Fatalf("dial control: %v", err)
	}
	if err := tunnel.WriteFrame(conn, tunnel.RegisterRequest{SessionID: sessionID, ClientVersion: "vtest"}); err != nil {
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
	srv := &http.Server{Handler: fleetdMCPHandler()}
	go func() { _ = srv.Serve(sess) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = sess.Close()
		_ = conn.Close()
	})
	return reply
}

// publicIDOf extracts the <id> from a https://host/mcp/<id> public URL.
func publicIDOf(t *testing.T, publicURL string) string {
	t.Helper()
	u, err := url.Parse(publicURL)
	if err != nil {
		t.Fatalf("parse public url: %v", err)
	}
	return strings.TrimPrefix(u.Path, "/mcp/")
}

func httpsClient(pool *x509.CertPool) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
	}}
}

func waitRegistered(t *testing.T, s *Server, publicID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.reg.lookup(publicID) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session never registered")
}

func TestGatewayEndToEnd(t *testing.T) {
	cert, pool := genTestTLS(t)
	s, controlAddr, publicAddr := startTestGateway(t, cert, "https://gw.example.com")

	reply := dialFleetd(t, controlAddr, pool, "")
	id := publicIDOf(t, reply.PublicURL)
	if strings.Contains(reply.PublicURL, reply.SessionID) {
		t.Fatal("public URL must not contain the reclaim secret")
	}
	waitRegistered(t, s, id)

	client := httpsClient(pool)
	base := "https://" + publicAddr + "/mcp/" + id

	// Exact endpoint maps to fleetd root.
	if body := getBody(t, client, base, ""); body != "root" {
		t.Fatalf("/mcp/<id> -> %q, want root", body)
	}
	// Subpath + Authorization passthrough.
	req, _ := http.NewRequest(http.MethodGet, base+"/echo", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "Bearer secret-token" {
		t.Fatalf("auth not forwarded through tunnel: %q", b)
	}

	// SSE streams end to end.
	sseResp, err := client.Get(base + "/sse?n=5")
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	defer sseResp.Body.Close()
	events := 0
	sc := bufio.NewScanner(sseResp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			events++
		}
	}
	if events != 5 {
		t.Fatalf("got %d SSE events through the gateway, want 5", events)
	}
}

func TestGatewayUnknownSession404(t *testing.T) {
	cert, pool := genTestTLS(t)
	_, _, publicAddr := startTestGateway(t, cert, "https://gw.example.com")
	client := httpsClient(pool)
	resp, err := client.Get("https://" + publicAddr + "/mcp/deadbeefdeadbeef")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session -> %d, want 404", resp.StatusCode)
	}
}

func TestGatewayStickyReconnect(t *testing.T) {
	cert, pool := genTestTLS(t)
	s, controlAddr, _ := startTestGateway(t, cert, "https://gw.example.com")

	reply1 := dialFleetd(t, controlAddr, pool, "")
	waitRegistered(t, s, publicIDOf(t, reply1.PublicURL))

	// Reconnect presenting the secret -> the SAME public URL (sticky).
	reply2 := dialFleetd(t, controlAddr, pool, reply1.SessionID)
	if reply2.PublicURL != reply1.PublicURL {
		t.Fatalf("sticky reconnect: got %q, want %q", reply2.PublicURL, reply1.PublicURL)
	}
	if reply2.SessionID != reply1.SessionID {
		t.Fatalf("sticky reconnect: secret changed %q -> %q", reply1.SessionID, reply2.SessionID)
	}
}

func getBody(t *testing.T, c *http.Client, url, auth string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
