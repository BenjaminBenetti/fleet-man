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
// and, for https, the TLS server name to verify. The URL must be http or https;
// the port defaults to 443 for https and 80 for http when not given. serverName
// is the host for https and EMPTY for http — an empty serverName is the signal
// to dial plaintext (TLS is terminated upstream by a reverse proxy). It is split
// out (and pure) so the URL handling is unit testable without a network.
func gatewayAddress(gatewayURL string) (addr, serverName string, err error) {
	u, err := url.Parse(gatewayURL)
	if err != nil {
		return "", "", fmt.Errorf("parse gateway url: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return "", "", fmt.Errorf("gateway url has no host: %q", gatewayURL)
	}
	port := u.Port()
	switch u.Scheme {
	case "https":
		if port == "" {
			port = "443"
		}
		serverName = host
	case "http":
		if port == "" {
			port = "80"
		}
		serverName = "" // plaintext: TLS is terminated upstream
	default:
		return "", "", fmt.Errorf("gateway url must be http or https, got %q", u.Scheme)
	}
	return net.JoinHostPort(host, port), serverName, nil
}

// dialGateway is the production transport seam (Manager.dial). For an https URL
// it TLS-dials, verified against the OS system roots; for an http URL it dials
// plaintext TCP (TLS terminated upstream by a reverse proxy). Tests override
// Manager.dial to reach an in-process gateway.
func dialGateway(ctx context.Context, gatewayURL string) (net.Conn, error) {
	addr, serverName, err := gatewayAddress(gatewayURL)
	if err != nil {
		return nil, err
	}
	if serverName == "" {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("dial gateway %s: %w", addr, err)
		}
		return conn, nil
	}
	d := &tls.Dialer{Config: &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial gateway %s: %w", addr, err)
	}
	return conn, nil
}
