package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/gateway"
	"github.com/BenjaminBenetti/fleet-man/internal/server/remote"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"google.golang.org/grpc/credentials"
)

// webhook_remote_e2e_test.go is the full-stack integration test for the webhook
// delivery feature (issue #193): a REAL webhook receiver (svc.webhookHandler) is
// exposed through a REAL gateway by a REAL remote.Manager tunnel, and a REAL HTTP
// client POSTs an event to the gateway-minted public webhook URL. A success
// proves the whole path — HTTPS → gateway /webhook route → TagWebhook tunnel
// stream → fleetd demux → webhook receiver → trigger match → scheduler enqueue.

// webhookStack stands up the full real stack with one webhook trigger ("ci",
// regex `"action":"opened"`) and returns the gateway-minted public webhook base
// URL, a cert pool trusting the gateway, and the service (whose webhookFires
// channel the test drains to confirm a fire).
func webhookStack(t *testing.T) (publicWebhookBase string, pool *x509.CertPool, svc *service) {
	t.Helper()
	isolateFleetDir(t)
	if err := os.MkdirAll(fleetpaths.Dir(), 0o700); err != nil {
		t.Fatalf("mkdir fleet dir: %v", err)
	}

	svc = newService()
	// Stub the persisted state with a single webhook trigger. The receiver loads
	// via scheduleLoadState, so this drives matching without touching disk.
	origLoad := scheduleLoadState
	scheduleLoadState = func() (*state.State, error) {
		return &state.State{Fleets: map[string]*fleet.Fleet{
			"alpha": {Name: "alpha", Settings: fleet.FleetSettings{
				Agents: []fleet.Agent{{Name: "a"}},
				Triggers: []fleet.Trigger{{
					Name: "ci", Type: fleet.TriggerWebhook, AgentNames: []string{"a"},
					WebhookName: "ci", FilterType: fleet.WebhookFilterRegex, Regex: `"action":"opened"`,
				}},
			}},
		}}, nil
	}
	t.Cleanup(func() { scheduleLoadState = origLoad })

	// The tunnel requires the local MCP server up (its loopback port), even though
	// this test only exercises webhooks.
	mcpHTTP, mcpPort := startMCPServer(svc)
	if mcpHTTP == nil {
		t.Fatal("MCP server failed to start")
	}
	t.Cleanup(func() { _ = mcpHTTP.Close() })

	// Real webhook receiver served over the in-memory tunnel listener.
	webhookLis := remote.NewChanListener()
	webhookSrv := &http.Server{Handler: svc.webhookHandler()}
	go func() { _ = webhookSrv.Serve(webhookLis) }()
	t.Cleanup(func() { _ = webhookSrv.Close() })

	// Real gateway on ephemeral TLS listeners.
	var certPath, keyPath string
	pool, certPath, keyPath = genTestTLSFiles(t)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load keypair: %v", err)
	}
	serverTLS := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	publicLn, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("public listen: %v", err)
	}
	t.Cleanup(func() { _ = publicLn.Close() })
	grpcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grpc listen: %v", err)
	}
	t.Cleanup(func() { _ = grpcLn.Close() })
	publicBase := "https://" + publicLn.Addr().String()

	gw, err := gateway.New(gateway.Config{PublicURL: publicBase, TLSCert: certPath, TLSKey: keyPath})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = gw.ServeListeners(ctx, publicLn, grpcLn) }()

	// Real fleetd tunnel manager, webhook-only (MCP/fleet off).
	statusCh := make(chan *fleetgrpc.RemoteMcpStatus, 64)
	gwCreds := credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})
	mgr := remote.NewManager(mcpPort, "e2e",
		func(st *fleetgrpc.RemoteMcpStatus) { statusCh <- st },
		remote.WithDialFunc(registerDialFunc(grpcLn.Addr().String(), gwCreds)),
		remote.WithWebhookListener(webhookLis),
	)
	go mgr.Run(ctx)
	mgr.Reconcile(false, false, true, "https://gw.example.com")

	deadline := time.After(10 * time.Second)
	for {
		select {
		case st := <-statusCh:
			if st.GetState() == fleetgrpc.RemoteMcpConn_REMOTE_MCP_CONN_CONNECTED {
				if st.GetPublicWebhookUrl() == "" {
					t.Fatal("CONNECTED status missing public webhook URL")
				}
				return st.GetPublicWebhookUrl(), pool, svc
			}
		case <-deadline:
			t.Fatal("timed out waiting for the webhook tunnel to connect")
		}
	}
}

// TestWebhookDeliveryEndToEnd POSTs a matching event to the gateway's public
// webhook URL and confirms the trigger fires (a fire is enqueued to the scheduler).
func TestWebhookDeliveryEndToEnd(t *testing.T) {
	base, pool, svc := webhookStack(t)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}}}

	resp, err := client.Post(base+"/ci", "application/json", strings.NewReader(`{"action":"opened"}`))
	if err != nil {
		t.Fatalf("POST webhook through the tunnel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook POST -> %d, want 200", resp.StatusCode)
	}

	select {
	case batch := <-svc.webhookFires:
		if len(batch) != 1 || batch[0].fleet != "alpha" || batch[0].trigger.Name != "ci" {
			t.Fatalf("unexpected fire batch through the tunnel: %+v", batch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event delivered but no trigger fire was enqueued")
	}
}

// TestWebhookUnknownName404 confirms an event for a name no trigger carries is
// rejected end to end with 404 (and fires nothing).
func TestWebhookUnknownName404(t *testing.T) {
	base, pool, svc := webhookStack(t)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}}}

	resp, err := client.Post(base+"/does-not-exist", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST webhook: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown webhook name -> %d, want 404", resp.StatusCode)
	}
	if b := drainBatch(svc); b != nil {
		t.Fatalf("no fire expected for an unknown name, got %+v", b)
	}
}
