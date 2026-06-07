package remote

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
)

// dial.go opens the outbound control connection to the gateway. fleetd is behind
// NAT/firewall, so it always DIALS — the gateway never connects in.

// gatewayAddress resolves a user-entered gateway URL to the TCP address to dial
// and the TLS server name to verify. The URL must be https; the port defaults to
// 443 when not given. It is split out (and pure) so the URL handling is unit
// testable without a network.
func gatewayAddress(gatewayURL string) (addr, serverName string, err error) {
	u, err := url.Parse(gatewayURL)
	if err != nil {
		return "", "", fmt.Errorf("parse gateway url: %w", err)
	}
	if u.Scheme != "https" {
		return "", "", fmt.Errorf("gateway url must be https, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return "", "", fmt.Errorf("gateway url has no host: %q", gatewayURL)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(host, port), host, nil
}

// dialTLS is the production transport seam (Manager.dial): a TLS dial to the
// gateway verified against the system roots. Tests override Manager.dial to
// reach an in-process gateway with a test CA.
func dialTLS(ctx context.Context, gatewayURL string) (net.Conn, error) {
	addr, serverName, err := gatewayAddress(gatewayURL)
	if err != nil {
		return nil, err
	}
	d := &tls.Dialer{Config: &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial gateway %s: %w", addr, err)
	}
	return conn, nil
}
