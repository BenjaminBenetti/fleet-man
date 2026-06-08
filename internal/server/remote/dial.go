package remote

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"

	"github.com/BenjaminBenetti/fleet-man/internal/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// dial.go opens fleetd's outbound registration to the gateway. fleetd is behind
// NAT/firewall, so it always DIALS — it opens a gRPC bidi Register stream on the
// gateway's gRPC endpoint and wraps it as a net.Conn (tunnel.StreamConn), over
// which the register handshake and the yamux reverse tunnel then run (see
// internal/tunnel). There is NO dedicated TCP control port; registration shares the
// HTTP/2 gRPC endpoint, so the whole path is frontable by an L7 proxy.

// registerStreamDesc matches the gateway's Register bidi method.
var registerStreamDesc = &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}

// grpcTarget resolves a gateway URL to a gRPC dial target and whether to use TLS.
// The URL must be http or https; the port defaults to 443 (https) or 80 (http).
// https verifies the gateway against the OS system roots; http dials plaintext h2c
// (TLS terminated upstream by a reverse proxy). Split out (and pure) so the URL
// handling is unit-testable without a network.
func grpcTarget(gatewayURL string) (target string, useTLS bool, err error) {
	u, err := url.Parse(gatewayURL)
	if err != nil {
		return "", false, fmt.Errorf("parse gateway url: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return "", false, fmt.Errorf("gateway url has no host: %q", gatewayURL)
	}
	port := u.Port()
	switch u.Scheme {
	case "https":
		if port == "" {
			port = "443"
		}
		useTLS = true
	case "http":
		if port == "" {
			port = "80"
		}
		useTLS = false
	default:
		return "", false, fmt.Errorf("gateway url must be http or https, got %q", u.Scheme)
	}
	return "dns:///" + net.JoinHostPort(host, port), useTLS, nil
}

// dialGateway is the production transport seam (Manager.dial): it opens the gRPC
// Register stream to the gateway and returns it as a net.Conn for the handshake +
// yamux tunnel. https uses TLS verified against the system roots; http uses
// plaintext h2c. Tests override Manager.dial to reach an in-process gateway.
func dialGateway(ctx context.Context, gatewayURL string) (net.Conn, error) {
	target, useTLS, err := grpcTarget(gatewayURL)
	if err != nil {
		return nil, err
	}
	var creds credentials.TransportCredentials
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	} else {
		creds = insecure.NewCredentials()
	}
	cc, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("gateway client: %w", err)
	}
	stream, err := cc.NewStream(ctx, registerStreamDesc, tunnel.RegisterMethod, grpc.ForceCodec(tunnel.RawCodec{}))
	if err != nil {
		_ = cc.Close()
		return nil, fmt.Errorf("open register stream: %w", err)
	}
	// Closing the conn tears the stream down by closing the whole client connection.
	return tunnel.NewStreamConn(stream, func() { _ = cc.Close() }), nil
}
