package fleetclient

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestIsSSHURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"ssh://ben@desktop":             true,
		"  SSH://desktop:2222 ":         true,
		"https://gw.example.com/abc":    false,
		"http://gw.example.com:50051/x": false,
		"desktop":                       false,
		"":                              false,
	} {
		if got := IsSSHURL(raw); got != want {
			t.Errorf("IsSSHURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

// TestSelectEndpointSSH: FLEET_SSH resolves through the local daemon into a
// loopback target carrying the discovered token; FLEET_GATEWAY still wins when
// both are set (the TUI only ever sets one); the endpoint is remote.
func TestSelectEndpointSSH(t *testing.T) {
	t.Setenv(EnvGateway, "")
	t.Setenv(EnvServer, "")
	t.Setenv(EnvSSH, "ssh://ben@desktop")

	orig := resolveSSHRemote
	var resolved string
	resolveSSHRemote = func(_ context.Context, rawURL string) (string, string, error) {
		resolved = rawURL
		return "127.0.0.1:40001", "tok-remote", nil
	}
	defer func() { resolveSSHRemote = orig }()

	ep, err := selectEndpoint(context.Background())
	if err != nil {
		t.Fatalf("selectEndpoint: %v", err)
	}
	if resolved != "ssh://ben@desktop" {
		t.Fatalf("resolved %q, want the FLEET_SSH url", resolved)
	}
	if ep.Target() != "dns:///127.0.0.1:40001" || ep.IsLocal() || ep.String() != "ssh://ben@desktop" {
		t.Fatalf("endpoint = %s / target %s / local %v", ep, ep.Target(), ep.IsLocal())
	}
	sshEp, ok := ep.(sshEndpoint)
	if !ok || sshEp.token != "tok-remote" {
		t.Fatalf("endpoint should carry the discovered token: %+v", ep)
	}
	md, err := gatewayPerRPC{token: sshEp.token}.GetRequestMetadata(context.Background())
	if err != nil || md["authorization"] != "Bearer tok-remote" {
		t.Fatalf("per-RPC metadata = %v, %v", md, err)
	}
	if !IsRemote() || !IsSSH() || IsGateway() {
		t.Fatalf("IsRemote=%v IsSSH=%v IsGateway=%v", IsRemote(), IsSSH(), IsGateway())
	}

	t.Setenv(EnvGateway, "https://gw.example.com/abc")
	if ep, err := selectEndpoint(context.Background()); err != nil || ep.String() != "https://gw.example.com/abc" {
		t.Fatalf("FLEET_GATEWAY should take precedence: %v, %v", ep, err)
	}
}

func TestSelectEndpointSSHErrors(t *testing.T) {
	t.Setenv(EnvGateway, "")
	t.Setenv(EnvServer, "")

	t.Setenv(EnvSSH, "desktop")
	if _, err := selectEndpoint(context.Background()); err == nil || !strings.Contains(err.Error(), "ssh://") {
		t.Fatalf("malformed FLEET_SSH: %v", err)
	}

	t.Setenv(EnvSSH, "ssh://desktop")
	orig := resolveSSHRemote
	resolveSSHRemote = func(context.Context, string) (string, string, error) {
		return "", "", errors.New("ssh: Permission denied (publickey).")
	}
	defer func() { resolveSSHRemote = orig }()
	if _, err := selectEndpoint(context.Background()); err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("resolve failure should surface verbatim: %v", err)
	}
}
