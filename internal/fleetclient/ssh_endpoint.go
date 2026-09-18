package fleetclient

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"google.golang.org/grpc"
)

// ssh_endpoint.go reaches a daemon over an SSH tunnel, with NO gateway. The
// LOCAL daemon owns the tunnel (internal/server/sshtunnel): it ssh-forwards a
// loopback port to the remote daemon's token-gated gRPC listener and discovers
// the remote's bearer token over the same SSH connection. So an ssh endpoint
// is resolved, not parsed: the local daemon (ResolveArmadaRemote, a local-only
// RPC) hands out the current loopback address + token, bringing the tunnel up
// or rebuilding a dead one as a side effect.
//
// The address is NOT baked into the gRPC target. The local daemon restarts
// (a "Restart daemon" action, a version upgrade, a crash) and comes back with
// no tunnels, then rebuilds them on FRESH loopback ports on the next resolve —
// so a long-lived client connection that reconnected to its original port
// would dial a dead socket forever (the TUI caches one mutation connection for
// its whole life). Instead the endpoint installs a custom dialer that resolves
// through the local daemon on EVERY (re)connect: gRPC's own reconnect after a
// dropped transport lands on the tunnel's current port, and the bearer token
// is refreshed alongside. The first connect reuses the address the endpoint
// was built with (one resolve, not two, per Dial).

// SSHResolveTimeout bounds one resolve: the local daemon may have to auto-spawn,
// then ssh (connect + auth + a possible remote daemon start) and verify — the
// daemon's own bring-up timeouts (discovery, forward readiness, Hello) sum to
// under this, so a failure is always reported with ITS reason rather than as a
// bare client-side timeout. The caller's ctx may still be shorter; then the
// client just stops waiting while the daemon finishes the bring-up in the
// background, and the next dial finds the tunnel up. Exported so the TUI can
// size its connection-test / ping budget for ssh remotes to match.
const SSHResolveTimeout = 90 * time.Second

// sshTarget is the placeholder gRPC target for an ssh endpoint: the passthrough
// resolver hands it to the custom dialer unchanged, and the dialer ignores it
// in favour of the address it resolves through the local daemon.
const sshTarget = "passthrough:///fleet-ssh"

// sshEndpoint is a resolved ssh:// remote. Not auto-spawnable (the daemon is on
// another machine); IsLocal is false so the version handshake treats a mismatch
// as a hard error. The pointer fields are shared by every ClientConn built from
// the endpoint (a value type), so the dialer's refreshes reach the per-RPC
// credentials.
type sshEndpoint struct {
	rawURL string     // the ssh:// URL, for String()
	state  *sshTunnel // current loopback address + token, refreshed per connect
}

// sshTunnel is the mutable, shared view of the tunnel as last resolved.
type sshTunnel struct {
	mu    sync.Mutex
	addr  string // loopback host:port of the local daemon's forward
	token string // the remote daemon's bearer token
	// fresh is true right after a resolve whose address has not been dialed
	// yet; the dialer consumes it so the initial connect skips a second resolve.
	fresh bool
}

func (s *sshTunnel) snapshot() (addr, token string, fresh bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	addr, token, fresh = s.addr, s.token, s.fresh
	s.fresh = false
	return
}

func (s *sshTunnel) set(addr, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addr, s.token, s.fresh = addr, token, false
}

// resolveSSHRemote asks the LOCAL daemon for the tunnel endpoint of rawURL.
// Package var so the TUI/CLI tests can stub the round trip.
var resolveSSHRemote = func(ctx context.Context, rawURL string) (addr, token string, err error) {
	rctx, cancel := context.WithTimeout(ctx, SSHResolveTimeout)
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
// local daemon (which parses it strictly and brings the tunnel up). Resolving
// here — rather than only inside the dialer — surfaces a tunnel failure as its
// own descriptive error (ssh's diagnostic, "SSH mode is off", …) with its gRPC
// status intact, instead of buried inside a transport Unavailable.
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
	return sshEndpoint{rawURL: rawURL, state: &sshTunnel{addr: addr, token: token, fresh: true}}, nil
}

// Target is a placeholder: the dialer supplies the real loopback address.
func (e sshEndpoint) Target() string { return sshTarget }

// IsLocal is false — no auto-spawn / version-restart for a remote daemon.
func (e sshEndpoint) IsLocal() bool { return false }

// String is the ssh URL (which carries no secret).
func (e sshEndpoint) String() string { return e.rawURL }

// DialOptions: plaintext to the loopback forward (ssh is the encrypted hop),
// the re-resolving dialer, and per-RPC credentials that always send the token
// the last resolve returned.
func (e sshEndpoint) DialOptions() []grpc.DialOption {
	return append(insecureCreds(),
		grpc.WithContextDialer(e.dial),
		grpc.WithPerRPCCredentials(sshPerRPC{state: e.state}))
}

// dial opens the TCP connection for one gRPC transport. The first connect uses
// the address the endpoint was resolved with; every later one (gRPC
// reconnecting after the transport dropped) re-resolves through the local
// daemon, which is what moves a live client onto the tunnel's new port after a
// local daemon restart. A failed resolve is returned as the dial error, so gRPC
// retries with its usual backoff and the RPC reports Unavailable with the
// reason inside.
func (e sshEndpoint) dial(ctx context.Context, _ string) (net.Conn, error) {
	addr, _, fresh := e.state.snapshot()
	if !fresh {
		var err error
		var token string
		addr, token, err = resolveSSHRemote(ctx, e.rawURL)
		if err != nil {
			return nil, err
		}
		e.state.set(addr, token)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// sshPerRPC attaches the bearer token from the latest resolve to every RPC.
// RequireTransportSecurity is false: the hop is a loopback socket into an ssh
// forward, encrypted by ssh itself.
type sshPerRPC struct{ state *sshTunnel }

func (p sshPerRPC) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	p.state.mu.Lock()
	token := p.state.token
	p.state.mu.Unlock()
	return map[string]string{"authorization": "Bearer " + token}, nil
}

func (sshPerRPC) RequireTransportSecurity() bool { return false }
