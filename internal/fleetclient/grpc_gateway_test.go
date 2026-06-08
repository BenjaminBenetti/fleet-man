package fleetclient

import (
	"strings"
	"testing"
)

func TestNewGatewayEndpoint(t *testing.T) {
	cases := []struct {
		url        string
		wantTarget string
		wantTLS    bool
		wantID     string
		wantErr    bool
	}{
		// https defaults to :443, verified TLS; id is the last path segment.
		{url: "https://gw.example.com/abc123", wantTarget: "dns:///gw.example.com:443", wantTLS: true, wantID: "abc123"},
		// explicit gRPC port.
		{url: "https://gw.example.com:50051/abc123", wantTarget: "dns:///gw.example.com:50051", wantTLS: true, wantID: "abc123"},
		// http -> plaintext h2c, default :80.
		{url: "http://gw.example.com/abc123", wantTarget: "dns:///gw.example.com:80", wantTLS: false, wantID: "abc123"},
		// a /grpc/<id> path is also accepted (last segment wins).
		{url: "https://gw.example.com:50051/grpc/abc123", wantTarget: "dns:///gw.example.com:50051", wantTLS: true, wantID: "abc123"},
		{url: "ftp://gw/abc", wantErr: true},            // only http/https
		{url: "https:///abc", wantErr: true},            // no host
		{url: "https://gw.example.com", wantErr: true},  // no id path
		{url: "https://gw.example.com/", wantErr: true}, // empty id
		{url: "://bad", wantErr: true},                  // unparseable
	}
	for _, c := range cases {
		ep, err := newGatewayEndpoint(c.url, "tok")
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got %+v", c.url, ep)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.url, err)
			continue
		}
		if ep.target != c.wantTarget || ep.useTLS != c.wantTLS || ep.id != c.wantID {
			t.Errorf("%q: got (target=%q tls=%v id=%q), want (%q %v %q)",
				c.url, ep.target, ep.useTLS, ep.id, c.wantTarget, c.wantTLS, c.wantID)
		}
		if ep.token != "tok" {
			t.Errorf("%q: token = %q, want tok", c.url, ep.token)
		}
	}
}

// TestGatewayPerRPCMetadata verifies every RPC carries the routing id and bearer
// token, and that the creds are allowed over a plaintext transport.
func TestGatewayPerRPCMetadata(t *testing.T) {
	g := gatewayPerRPC{token: "secret", sessionID: "abc123"}
	md, err := g.GetRequestMetadata(t.Context())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	if md["authorization"] != "Bearer secret" {
		t.Fatalf("authorization = %q, want Bearer secret", md["authorization"])
	}
	if md[grpcSessionHeader] != "abc123" {
		t.Fatalf("%s = %q, want abc123", grpcSessionHeader, md[grpcSessionHeader])
	}
	if g.RequireTransportSecurity() {
		t.Fatal("per-RPC creds must not require transport security (h2c plaintext path)")
	}
}

func TestSelectEndpointGatewayPrecedence(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "https://gw.example.com:50051/abc123")
	t.Setenv("FLEET_SERVER", "1.2.3.4:9000") // gateway must win
	t.Setenv("FLEET_TOKEN", "tok")

	ep, err := selectEndpoint()
	if err != nil {
		t.Fatalf("selectEndpoint: %v", err)
	}
	ge, ok := ep.(gatewayEndpoint)
	if !ok {
		t.Fatalf("FLEET_GATEWAY should select a gatewayEndpoint, got %T", ep)
	}
	if ep.IsLocal() {
		t.Fatal("gateway endpoint must not be local (no auto-spawn)")
	}
	if ge.token != "tok" || ge.id != "abc123" {
		t.Fatalf("token=%q id=%q, want tok / abc123", ge.token, ge.id)
	}
	if !strings.Contains(ep.String(), "gw.example.com") || strings.Contains(ep.String(), "tok") {
		t.Fatalf("String() should show the URL without the token: %q", ep.String())
	}
}

// TestSelectEndpointBadGatewayErrors verifies a malformed FLEET_GATEWAY surfaces
// an error rather than a silently-broken endpoint.
func TestSelectEndpointBadGateway(t *testing.T) {
	t.Setenv("FLEET_GATEWAY", "ftp://nope")
	if _, err := selectEndpoint(); err == nil {
		t.Fatal("malformed FLEET_GATEWAY should error")
	}
}

func TestGatewayTokenFromEnv(t *testing.T) {
	t.Setenv("FLEET_TOKEN", "  env-token  ")
	if got := gatewayToken(); got != "env-token" {
		t.Fatalf("gatewayToken() = %q, want trimmed env-token", got)
	}
}
