package fleetclient

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// remote.go names a remote daemon as a value, apart from the environment
// that chose it: `fleet code` records the remote its FLEET_GATEWAY,
// FLEET_SSH or FLEET_SERVER reached (CurrentRemote), and `fleet ssh-proxy`,
// which OpenSSH starts without those variables, dials it again from that
// record (DialRemote).

// RemoteKind is how a remote daemon is reached.
type RemoteKind string

const (
	// RemoteGateway: Address is a FLEET_GATEWAY URL.
	RemoteGateway RemoteKind = "gateway"
	// RemoteSSH: Address is a FLEET_SSH ssh:// URL, whose tunnel the local
	// daemon keeps.
	RemoteSSH RemoteKind = "ssh"
	// RemoteServer: Address is a FLEET_SERVER host:port.
	RemoteServer RemoteKind = "server"
)

// Remote is one remote daemon.
type Remote struct {
	Kind    RemoteKind
	Address string
	// Token is the bearer token for a gateway when it was given explicitly
	// (FLEET_TOKEN); empty, DialRemote falls back as Dial does.
	Token string
}

// CurrentRemote is the remote daemon the endpoint variables point the
// client at, with selectEndpoint's precedence (FLEET_GATEWAY, FLEET_SSH,
// FLEET_SERVER), or false for the local daemon. Its Token is FLEET_TOKEN's
// (never the on-disk token a same-host gateway client falls back to).
func CurrentRemote() (Remote, bool) {
	if gw := strings.TrimSpace(os.Getenv(EnvGateway)); gw != "" {
		return Remote{Kind: RemoteGateway, Address: gw, Token: strings.TrimSpace(os.Getenv(EnvToken))}, true
	}
	if ssh := strings.TrimSpace(os.Getenv(EnvSSH)); ssh != "" {
		return Remote{Kind: RemoteSSH, Address: ssh}, true
	}
	if addr := strings.TrimSpace(os.Getenv(EnvServer)); addr != "" {
		return Remote{Kind: RemoteServer, Address: addr}, true
	}
	return Remote{}, false
}

// DialRemote connects to r as Dial connects to the daemon the variables
// naming r would choose: a gateway with r's token (else FLEET_TOKEN, else
// ~/.fleet/mcp.token), an ssh:// remote through the local daemon's tunnel,
// a plain TCP address. Like every remote endpoint it never spawns a daemon,
// and a version mismatch is an error.
func DialRemote(ctx context.Context, r Remote) (*Conn, error) {
	var (
		ep  Endpoint
		err error
	)
	switch r.Kind {
	case RemoteGateway:
		token := r.Token
		if token == "" {
			token = gatewayToken()
		}
		ep, err = newGatewayEndpoint(r.Address, token)
	case RemoteSSH:
		ep, err = newSSHEndpoint(ctx, r.Address)
	case RemoteServer:
		ep = remoteEndpoint{addr: r.Address}
	default:
		return nil, fmt.Errorf("unknown remote kind %q", r.Kind)
	}
	if err != nil {
		return nil, err
	}
	return dialEndpoint(ctx, ep)
}
