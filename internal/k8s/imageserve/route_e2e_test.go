package imageserve_test

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/gateway"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/imageserve"
)

// route_e2e_test.go is `fleet cluster test`'s daemon-side image delivery
// check end to end: fleetd fetches the probe image's manifest through a real
// TLS gateway's public URL, with the connection's credential, the way a node
// pulls, and each broken step is named: TLS, credential, digest, route.

// publicTransport dials the gateway's public listener whatever host the
// request names (the registry host is the --public-url's, here
// gw.example.com), verifying TLS against pool (nil: the system roots).
func publicTransport(addr string, ca *testCA) *http.Transport {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca != nil {
		cfg.RootCAs = ca.pool
	}
	return &http.Transport{
		TLSClientConfig: cfg,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
}

// staticTunnel is a Tunnel with a fixed registry prefix.
type staticTunnel string

func (s staticTunnel) Registry() (string, string) { return string(s), "" }
func (staticTunnel) WarmRegistry()                {}

// wrongCredential is Clusters with every credential replaced.
type wrongCredential struct{ imageserve.Clusters }

func (w wrongCredential) Lookup(id string) (imageserve.Cluster, bool) {
	c, ok := w.Clusters.Lookup(id)
	c.Credential = "not-the-credential"
	return c, ok
}

func TestCheckRouteThroughTheGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ca := newTestCA(t, "gw.example.com", net.ParseIP("127.0.0.1"))
	rawPublic, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicAddr := rawPublic.Addr().String()
	_, grpcAddr, stop := startGateway(t, gateway.Config{PublicURL: "https://gw.example.com", SessionKey: "route-key"},
		tls.NewListener(rawPublic, &tls.Config{Certificates: []tls.Certificate{ca.leaf}, MinVersion: tls.VersionTLS12}), nil)
	f := startFleetd(t, fleetdOptions{dial: registerDial(grpcAddr, nil), gatewayURL: "https://gw.example.com"})
	prefix := f.waitPrefix(t)
	f.images.Transport = publicTransport(publicAddr, &ca)

	target := f.target("kc-1")
	target.Secrets = nil
	probe, err := f.images.Probe(context.Background(), target)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !strings.HasPrefix(probe.Ref, prefix+"/devcontainer@") {
		t.Fatalf("probe ref %s, want it under %s", probe.Ref, prefix)
	}
	check := func(g *imageserve.Gateway, cluster, digest string) imageserve.RouteCheck {
		t.Helper()
		// The gateway binds the session a moment after its reply.
		var rc imageserve.RouteCheck
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if rc = g.CheckRoute(context.Background(), cluster, digest); rc.Step != imageserve.StepRoute || !strings.Contains(rc.Message, "404") {
				break
			}
		}
		return rc
	}

	// Route, TLS and credential: the manifest comes back.
	if rc := check(f.images, "kc-1", probe.Digest); rc.Step != "" || rc.Host != "gw.example.com" {
		t.Fatalf("a working route: %+v", rc)
	}
	// An unsaved connection has no credential: fleetd's own challenge
	// proves the route and TLS.
	if rc := check(f.images, "", probe.Digest); rc.Step != "" {
		t.Fatalf("route without a credential: %+v", rc)
	}
	if _, ok := f.images.Handler().LastPull("kc-1"); ok {
		t.Fatal("the route check counted as a pull")
	}
	// A digest the connection does not serve.
	if rc := check(f.images, "kc-1", "sha256:"+strings.Repeat("0", 64)); rc.Step != imageserve.StepDigest {
		t.Fatalf("an unserved digest: %+v", rc)
	}
	// A credential fleetd does not know.
	wrong := &imageserve.Gateway{Store: f.images.Store, Tunnel: f.mgr, Clusters: wrongCredential{f.clusters}, Transport: f.images.Transport}
	if rc := check(wrong, "kc-1", probe.Digest); rc.Step != imageserve.StepCredential {
		t.Fatalf("a refused credential: %+v", rc)
	}
	// The daemon host does not trust the gateway's certificate.
	untrusted := &imageserve.Gateway{Store: f.images.Store, Tunnel: f.mgr, Clusters: f.clusters, Transport: publicTransport(publicAddr, nil)}
	if rc := check(untrusted, "kc-1", probe.Digest); rc.Step != imageserve.StepTLS || !strings.Contains(rc.Message, "certificate") {
		t.Fatalf("an untrusted certificate: %+v", rc)
	}
	// A registry id the gateway does not route (it forgot the session).
	forgotten := &imageserve.Gateway{Store: f.images.Store, Tunnel: staticTunnel("gw.example.com/" + strings.Repeat("0", 64)), Clusters: f.clusters, Transport: f.images.Transport}
	if rc := forgotten.CheckRoute(context.Background(), "kc-1", probe.Digest); rc.Step != imageserve.StepRoute || !strings.Contains(rc.Message, "404") {
		t.Fatalf("an unknown registry id: %+v", rc)
	}
	// The gateway is down.
	stop()
	if rc := f.images.CheckRoute(context.Background(), "kc-1", probe.Digest); rc.Step != imageserve.StepRoute {
		t.Fatalf("the gateway stopped: %+v", rc)
	}
}
