package sshtunnel

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
	"github.com/BenjaminBenetti/fleet-man/internal/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// helloTimeout bounds the end-to-end check through an (allegedly) live forward.
// Generous: it rides an established ssh session, so it is one RTT + the remote
// daemon's Hello.
const helloTimeout = 5 * time.Second

// Manager owns the SSH forwards, one per canonical ssh:// URL. All methods are
// safe for concurrent use; bring-up is serialized PER REMOTE, so a burst of
// client dials (the TUI opens its Watch stream, a mutation conn, and a status
// ping at once) shares a single ssh spawn rather than racing three.
type Manager struct {
	ctx     context.Context // daemon lifetime: cancelling it kills every forward
	mu      sync.Mutex
	tunnels map[string]*tunnel

	// Seams (production defaults; tests swap them for in-process fakes).
	discover func(ctx context.Context, t Target) (Discovery, error)
	forward  func(ctx context.Context, t Target, localPort, remotePort int) (forwardProc, error)
	hello    func(ctx context.Context, addr, token string) error
}

// tunnel is the state of one remote's forward. proc is nil while down; once a
// forward is up localPort is kept stable across rebuilds (only a bind failure
// moves it), so a client that cached the address survives a remote restart.
type tunnel struct {
	mu        sync.Mutex
	target    Target
	localPort int
	remote    Discovery
	proc      forwardProc
}

// New returns a Manager whose forwards live until ctx is cancelled (or Close).
func New(ctx context.Context) *Manager {
	return &Manager{
		ctx:      ctx,
		tunnels:  make(map[string]*tunnel),
		discover: discoverOverSSH,
		forward:  startForward,
		hello:    helloThrough,
	}
}

// Endpoint is what a client needs to reach a remote: the loopback address of
// the forward and the remote daemon's bearer token.
type Endpoint struct {
	Addr  string
	Token string
}

// Resolve returns a verified, dialable endpoint for rawURL, bringing the
// forward up (or rebuilding a dead/stale one) as needed:
//
//  1. forward already up and a Hello through it succeeds → return it;
//  2. otherwise discover the remote port + token over ssh, spawn the forward
//     (retrying a local bind collision on a fresh port), wait for it to bind,
//     and verify with a Hello.
//
// A Hello failure on a live forward is how a REMOTE daemon restart (new port)
// is noticed: the stale forward is torn down and rebuilt from a fresh
// discovery. Errors are worded for the settings page's status column.
func (m *Manager) Resolve(ctx context.Context, rawURL string) (Endpoint, error) {
	t, err := ParseURL(rawURL)
	if err != nil {
		return Endpoint{}, err
	}
	tn := m.tunnelFor(t)
	tn.mu.Lock()
	defer tn.mu.Unlock()

	if tn.proc != nil {
		select {
		case <-tn.proc.Done():
			flog.Warn("ssh tunnel exited; rebuilding", "remote", t.String(), "err", tn.proc.Err())
			tn.proc = nil
		default:
			ep := Endpoint{Addr: loopback(tn.localPort), Token: tn.remote.Token}
			if err := m.hello(ctx, ep.Addr, ep.Token); err == nil {
				return ep, nil
			} else {
				flog.Warn("ssh tunnel stale; rebuilding", "remote", t.String(), "err", err)
			}
			tn.proc.Kill()
			tn.proc = nil
		}
	}

	disc, err := m.discover(ctx, t)
	if err != nil {
		return Endpoint{}, fmt.Errorf("%s: %w", t.Host, err)
	}
	// Bring-up, retried on a fresh local port when ssh could not bind the chosen
	// one. The port is checked free BEFORE spawning (a squatter would otherwise
	// accept our readiness probe and ssh's bind failure would go unnoticed until
	// the Hello), and a failed Hello re-checks ssh's stderr for the same reason —
	// a squatter that arrived during ssh's connect+auth window.
	ep := Endpoint{Token: disc.Token}
	for attempt := 0; ; attempt++ {
		if tn.localPort == 0 || !portFree(tn.localPort) {
			port, err := freePort()
			if err != nil {
				return Endpoint{}, fmt.Errorf("pick local port: %w", err)
			}
			tn.localPort = port
		}
		ep.Addr = loopback(tn.localPort)
		proc, err := m.forward(m.ctx, t, tn.localPort, disc.Port)
		if err != nil {
			return Endpoint{}, err
		}
		err = waitForwardReady(ctx, proc, tn.localPort)
		if err == nil {
			if err = m.hello(ctx, ep.Addr, ep.Token); err != nil {
				err = fmt.Errorf("tunnel up but the remote daemon did not answer: %w", err)
			}
		}
		if err == nil {
			tn.proc = proc
			tn.remote = disc
			break
		}
		proc.Kill()
		if attempt < 2 && (isBindFailure(err.Error()) || isBindFailure(waitErr(proc))) {
			flog.Warn("ssh tunnel local port taken; retrying on another", "remote", t.String(), "port", tn.localPort)
			tn.localPort = 0
			continue
		}
		return Endpoint{}, err
	}
	flog.Info("ssh tunnel up", "remote", t.String(), "local", ep.Addr, "remotePort", disc.Port)
	return ep, nil
}

// waitErr waits (briefly) for a killed forward to exit and returns its stderr
// diagnostic, so a bind failure that raced our readiness probe is still seen.
func waitErr(proc forwardProc) string {
	select {
	case <-proc.Done():
	case <-time.After(2 * time.Second):
	}
	return proc.Err()
}

// tunnelFor returns (creating if needed) the tunnel record for t.
func (m *Manager) tunnelFor(t Target) *tunnel {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := t.String()
	tn, ok := m.tunnels[key]
	if !ok {
		tn = &tunnel{target: t}
		m.tunnels[key] = tn
	}
	return tn
}

// Prune tears down every forward whose remote is not in keep (the armada
// registry after an edit). Entries that don't parse are ignored.
func (m *Manager) Prune(keep []string) {
	want := make(map[string]bool, len(keep))
	for _, raw := range keep {
		if t, err := ParseURL(raw); err == nil {
			want[t.String()] = true
		}
	}
	m.mu.Lock()
	var drop []*tunnel
	for key, tn := range m.tunnels {
		if !want[key] {
			drop = append(drop, tn)
			delete(m.tunnels, key)
		}
	}
	m.mu.Unlock()
	for _, tn := range drop {
		tn.mu.Lock()
		if tn.proc != nil {
			tn.proc.Kill()
			tn.proc = nil
		}
		tn.mu.Unlock()
		flog.Info("ssh tunnel removed", "remote", tn.target.String())
	}
}

// Close kills every forward (daemon shutdown).
func (m *Manager) Close() {
	m.Prune(nil)
}

// bearer attaches `authorization: Bearer <token>` to every RPC (the remote's
// tunnel-facing server is token-gated). Same wire shape as fleetclient's, which
// the import boundary keeps on the other side.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

// helloThrough runs one Hello RPC against addr with token: the end-to-end proof
// that the forward reaches a live, matching daemon (ssh accepting the local
// connection alone proves nothing — it connects to the remote port lazily).
func helloThrough(ctx context.Context, addr, token string) error {
	if token == "" {
		return errors.New("no bearer token")
	}
	cc, err := grpc.NewClient("dns:///"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearer(token)))
	if err != nil {
		return err
	}
	defer cc.Close()
	hctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	_, err = fleetgrpc.NewFleetServiceClient(cc).Hello(hctx, &fleetgrpc.HelloRequest{ClientVersion: version.Version})
	return err
}
