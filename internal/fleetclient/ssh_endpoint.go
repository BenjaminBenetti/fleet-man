package fleetclient

import (
	"context"
	"fmt"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
)

// ssh_endpoint.go reaches a daemon over an SSH tunnel, with NO gateway. The
// LOCAL daemon owns the tunnel (internal/server/sshtunnel): it ssh-forwards a
// loopback port to the remote daemon's token-gated gRPC listener and discovers
// the remote's bearer token over the same SSH connection. So an ssh endpoint
// is resolved, not parsed — every dial asks the local daemon (ResolveArmadaRemote,
// a local-only RPC) for the current loopback address + token, which also makes
// the local daemon bring the tunnel up or rebuild a dead one. The resulting
// endpoint is then an ordinary plaintext-loopback gRPC target carrying the
// bearer token per RPC, exactly like the gateway endpoint minus the routing
// header.

// sshResolveTimeout bounds one resolve: the local daemon may have to auto-spawn,
// then ssh (connect + auth + a possible remote daemon start) and verify.
const sshResolveTimeout = 90 * time.Second

// sshEndpoint is a resolved ssh:// remote. Not auto-spawnable (the daemon is on
// another machine); IsLocal is false so the version handshake treats a mismatch
// as a hard error.
type sshEndpoint struct {
	rawURL string // the ssh:// URL, for String()
	addr   string // loopback host:port of the local daemon's forward
	token  string // the remote daemon's bearer token
}

// resolveSSHRemote asks the LOCAL daemon for the tunnel endpoint of rawURL.
// Package var so the TUI/CLI tests can stub the round trip.
var resolveSSHRemote = func(ctx context.Context, rawURL string) (addr, token string, err error) {
	rctx, cancel := context.WithTimeout(ctx, sshResolveTimeout)
	defer cancel()
	conn, err := DialLocal(rctx)
	if err != nil {
		return "", "", fmt.Errorf("local fleet daemon (owns the ssh tunnel): %w", err)
	}
	defer conn.Close()
	reply, err := conn.Service().ResolveArmadaRemote(rctx, &fleetgrpc.ResolveArmadaRemoteRequest{Url: rawURL})
	if err != nil {
		return "", "", err
	}
	return reply.GetAddr(), reply.GetToken(), nil
}

// newSSHEndpoint validates rawURL's shape cheaply, then resolves it through the
// local daemon (which parses it strictly and brings the tunnel up).
func newSSHEndpoint(ctx context.Context, rawURL string) (sshEndpoint, error) {
	if !IsSSHURL(rawURL) {
		return sshEndpoint{}, fmt.Errorf("FLEET_SSH must be an ssh://[user@]host[:port] URL, got %q", rawURL)
	}
	addr, token, err := resolveSSHRemote(ctx, rawURL)
	if err != nil {
		return sshEndpoint{}, err
	}
	if addr == "" {
		return sshEndpoint{}, fmt.Errorf("local daemon returned no tunnel address for %s", rawURL)
	}
	return sshEndpoint{rawURL: rawURL, addr: addr, token: token}, nil
}

// Target is the loopback forward (a plain TCP gRPC target).
func (e sshEndpoint) Target() string { return "dns:///" + e.addr }

// IsLocal is false — no auto-spawn / version-restart for a remote daemon.
func (e sshEndpoint) IsLocal() bool { return false }

// String is the ssh URL (which carries no secret).
func (e sshEndpoint) String() string { return e.rawURL }

// DialOptions: plaintext to the loopback forward (ssh is the encrypted hop),
// plus the bearer token the remote daemon's interceptor requires.
func (e sshEndpoint) DialOptions() []grpc.DialOption {
	return append(insecureCreds(), grpc.WithPerRPCCredentials(gatewayPerRPC{token: e.token}))
}
