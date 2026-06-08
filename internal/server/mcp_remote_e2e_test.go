package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/gateway"
	"github.com/BenjaminBenetti/fleet-man/internal/server/remote"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcp_remote_e2e_test.go is the full-stack integration test for the remote-MCP
// feature: a REAL fleetd MCP server (loopback, bearer-gated, real fleet tools) is
// exposed through a REAL gateway by a REAL remote.Manager tunnel, and driven by a
// REAL MCP SDK client over HTTPS at the gateway-minted public URL. It is the
// authoritative check that an agent can actually reach the fleet through the
// gateway — in particular that the MCP SDK's default DNS-rebinding protection
// does not reject tunneled requests.

// genTestTLSFiles writes a self-signed cert (valid for 127.0.0.1) to temp files
// and returns a pool that trusts it plus the cert/key paths.
func genTestTLSFiles(t *testing.T) (pool *x509.CertPool, certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fleet-e2e"},
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

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	pool = x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append cert")
	}
	return pool, certPath, keyPath
}

// bearerRT adds an Authorization: Bearer header to every request.
type bearerRT struct {
	token string
	base  http.RoundTripper
}

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// remoteMCPStack stands up the full real stack and returns the gateway-minted
// public MCP URL, the gateway's public base URL, the MCP bearer token, and a
// cert pool trusting the gateway.
func remoteMCPStack(t *testing.T) (publicMCPURL, publicBase, token string, pool *x509.CertPool) {
	t.Helper()
	isolateFleetDir(t)
	if err := os.MkdirAll(fleetpaths.Dir(), 0o700); err != nil {
		t.Fatalf("mkdir fleet dir: %v", err)
	}

	// Real fleetd MCP server: loopback, bearer-gated, real fleet tools.
	svc := newService()
	mcpHTTP, mcpPort := startMCPServer(svc)
	if mcpHTTP == nil {
		t.Fatal("MCP server failed to start")
	}
	t.Cleanup(func() { _ = mcpHTTP.Close() })
	var err error
	token, err = loadOrCreateMCPToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	// Real gateway on ephemeral TLS listeners.
	var certPath, keyPath string
	pool, certPath, keyPath = genTestTLSFiles(t)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load keypair: %v", err)
	}
	serverTLS := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	controlLn, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("control listen: %v", err)
	}
	t.Cleanup(func() { _ = controlLn.Close() })
	publicLn, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("public listen: %v", err)
	}
	t.Cleanup(func() { _ = publicLn.Close() })
	publicBase = "https://" + publicLn.Addr().String()

	gw, err := gateway.New(gateway.Config{PublicURL: publicBase, TLSCert: certPath, TLSKey: keyPath})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = gw.ServeListeners(ctx, controlLn, publicLn, nil) }()

	// Real fleetd tunnel manager, dialing the test gateway's control listener.
	statusCh := make(chan *fleetgrpc.RemoteMcpStatus, 64)
	controlAddr := controlLn.Addr().String()
	mgr := remote.NewManager(mcpPort, "e2e",
		func(st *fleetgrpc.RemoteMcpStatus) { statusCh <- st },
		remote.WithDialFunc(func(dctx context.Context, _ string) (net.Conn, error) {
			d := &tls.Dialer{Config: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}}
			return d.DialContext(dctx, "tcp", controlAddr)
		}),
	)
	go mgr.Run(ctx)
	mgr.Reconcile(true, "https://gw.example.com")

	// Wait for CONNECTED and the gateway-assigned public URL.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case st := <-statusCh:
			if st.GetState() == fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED {
				if st.GetPublicUrl() == "" {
					t.Fatal("CONNECTED status missing public URL")
				}
				return st.GetPublicUrl(), publicBase, token, pool
			}
		case <-deadline:
			t.Fatal("timed out waiting for the tunnel to connect")
		}
	}
}

// TestRemoteMCPEndToEnd drives a real MCP client through the full chain
// (HTTPS -> gateway -> reverse tunnel -> fleetd -> loopback MCP server) and calls
// a real tool. A success here proves the DNS-rebinding/Host handling is correct
// and the bearer token is forwarded untouched.
func TestRemoteMCPEndToEnd(t *testing.T) {
	publicMCPURL, _, token, pool := remoteMCPStack(t)

	httpClient := &http.Client{Transport: bearerRT{
		token: token,
		base:  &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}},
	}}
	transport := &mcp.StreamableClientTransport{Endpoint: publicMCPURL, HTTPClient: httpClient}
	client := mcp.NewClient(&mcp.Implementation{Name: "fleet-e2e", Version: "1"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("MCP connect through the tunnel failed (DNS-rebinding/Host or auth?): %v", err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fleet_version"})
	if err != nil {
		t.Fatalf("CallTool fleet_version through the tunnel: %v", err)
	}
	if res.IsError {
		t.Fatalf("fleet_version returned a tool error: %s", toolText(res))
	}
	if toolText(res) == "" {
		t.Fatal("fleet_version returned empty content through the tunnel")
	}
}

// TestRemoteMCPRejectsBadToken confirms the MCP bearer token remains the access
// boundary end to end: a request with no/!wrong token is rejected (401), even
// though it reached fleetd through the tunnel.
func TestRemoteMCPRejectsBadToken(t *testing.T) {
	publicMCPURL, _, _, pool := remoteMCPStack(t)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}}}

	req, _ := http.NewRequest(http.MethodGet, publicMCPURL, nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token through the tunnel -> %d, want 401", resp.StatusCode)
	}
}

// TestRemoteMCPUnknownSession404 confirms the gateway 404s an unknown public id.
func TestRemoteMCPUnknownSession404(t *testing.T) {
	_, publicBase, _, pool := remoteMCPStack(t)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}}}

	resp, err := client.Get(publicBase + "/mcp/" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session -> %d, want 404", resp.StatusCode)
	}
}
