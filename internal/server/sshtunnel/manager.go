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

const (
	// helloTimeout bounds the end-to-end check through an (allegedly) live
	// forward. Generous: it rides an established ssh session, so it is one RTT
	// + the remote daemon's Hello.
	helloTimeout = 5 * time.Second
	// killWait bounds how long a torn-down forward is waited for after Kill, so
	// its local port is actually released before it is checked or reused (a
	// SIGKILLed ssh exits at once; this is a backstop).
	killWait = 2 * time.Second
)

// Manager owns the SSH forwards, one per canonical ssh:// URL. All methods are
// safe for concurrent use. A bring-up runs in its OWN goroutine under the
// daemon's lifetime context and is single-flight per remote: a burst of client
// dials (the TUI opens its Watch stream, a mutation conn, and a status ping at
// once) shares one ssh spawn, and a caller whose own context expires while
// waiting (a short client deadline) simply stops waiting — the bring-up
// finishes anyway, and the next dial finds the tunnel up. Only the internal
// timeouts (discovery, forward readiness, Hello) bound a bring-up.
type Manager struct {
	ctx     context.Context // daemon lifetime: cancelling it kills every forward
	mu      sync.Mutex
	tunnels map[string]*tunnel
	// offered remembers, per remote, the host keys the daemon itself fetched
	// and reported as unknown (hostkey.go) — the only lines TrustHostKey will
	// append. Keyed by canonical URL, then known_hosts line; the value is the
	// known_hosts path the offer was made for.
	offered map[string]map[string]string

	// Seams (production defaults; tests swap them for in-process fakes).
	discover func(ctx context.Context, t Target) (Discovery, error)
	forward  func(ctx context.Context, t Target, localPort, remotePort int) (forwardProc, error)
	hello    func(ctx context.Context, addr, token string) error
}

// tunnel is the state of one remote's forward. proc is nil while down; once a
// forward is up localPort is kept stable across rebuilds (only a bind failure
// moves it), so a client that cached the address survives a remote restart.
// inflight is non-nil while a bring-up goroutine runs and is closed when it
// finishes; lastErr is that bring-up's outcome. removed marks a tunnel that
// Remove/Close dropped, so a bring-up that finishes afterwards discards its
// forward instead of installing it.
type tunnel struct {
	mu        sync.Mutex
	target    Target
	localPort int
	remote    Discovery
	proc      forwardProc
	inflight  chan struct{}
	lastErr   error
	removed   bool
}

// New returns a Manager whose forwards live until ctx is cancelled (or Close).
func New(ctx context.Context) *Manager {
	return &Manager{
		ctx:      ctx,
		tunnels:  make(map[string]*tunnel),
		offered:  make(map[string]map[string]string),
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

// errRemoved is returned to a waiter whose tunnel was removed mid-bring-up.
var errRemoved = errors.New("remote was removed from the armada")

// Resolve returns a verified, dialable endpoint for rawURL, bringing the
// forward up (or rebuilding a dead/stale one) as needed:
//
//  1. forward already up and a Hello through it succeeds → return it;
//  2. otherwise start (or join) the remote's bring-up — discover the remote
//     port + token over ssh, spawn the forward (retrying a local bind collision
//     on a fresh port), wait for it to bind, verify with a Hello — and wait for
//     it, for as long as ctx allows.
//
// The liveness Hello runs under the DAEMON's context (bounded by
// helloTimeout), so its verdict only ever reflects the tunnel: a caller whose
// ctx is already spent gets ctx.Err() back and the forward is left alone. A
// genuine Hello failure on a live forward is how a REMOTE daemon restart (new
// port) is noticed: the stale forward is torn down and rebuilt from a fresh
// discovery. Errors are worded for the settings page's status column.
func (m *Manager) Resolve(ctx context.Context, rawURL string) (Endpoint, error) {
	t, err := ParseURL(rawURL)
	if err != nil {
		return Endpoint{}, err
	}
	tn := m.tunnelFor(t)

	// Fast path: a live forward that still answers.
	tn.mu.Lock()
	proc, inflight := tn.proc, tn.inflight
	ep := Endpoint{Addr: loopback(tn.localPort), Token: tn.remote.Token}
	tn.mu.Unlock()
	if inflight == nil && proc != nil {
		if ctx.Err() != nil {
			return Endpoint{}, ctx.Err()
		}
		if alive(proc) {
			hctx, cancel := context.WithTimeout(m.ctx, helloTimeout)
			err := m.hello(hctx, ep.Addr, ep.Token)
			cancel()
			if err == nil {
				return ep, nil
			}
			flog.Warn("ssh tunnel stale; rebuilding", "remote", t.String(), "err", err)
		} else {
			flog.Warn("ssh tunnel exited; rebuilding", "remote", t.String(), "err", proc.Err())
		}
	}

	// Slow path: start the bring-up, or join the one already running. The
	// check that proc is still the forward we judged dead guards against a
	// concurrent caller that already rebuilt it.
	tn.mu.Lock()
	if tn.proc != nil && tn.proc == proc && tn.inflight == nil {
		killAndWait(tn.proc)
		tn.proc = nil
	}
	if tn.inflight == nil && tn.proc == nil {
		tn.inflight = make(chan struct{})
		go m.bringUp(tn, tn.inflight)
	}
	wait := tn.inflight
	tn.mu.Unlock()

	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return Endpoint{}, fmt.Errorf("%s: %w (the tunnel keeps coming up in the background; retry shortly)", t.Host, ctx.Err())
		}
	}

	tn.mu.Lock()
	defer tn.mu.Unlock()
	if tn.lastErr != nil {
		return Endpoint{}, tn.lastErr
	}
	if tn.proc == nil {
		return Endpoint{}, errRemoved
	}
	return Endpoint{Addr: loopback(tn.localPort), Token: tn.remote.Token}, nil
}

// bringUp establishes the forward for tn under the daemon's context and
// installs the outcome, then releases every waiter by closing done. It is the
// only writer of tn's forward state while it runs (Resolve only reads after
// done is closed; Remove marks removed and lets it finish).
func (m *Manager) bringUp(tn *tunnel, done chan struct{}) {
	tn.mu.Lock()
	t, localPort := tn.target, tn.localPort
	tn.mu.Unlock()

	proc, port, disc, err := m.establish(t, localPort)
	m.recordOffer(t, err)

	tn.mu.Lock()
	defer tn.mu.Unlock()
	defer close(done)
	tn.inflight = nil
	if tn.removed {
		if proc != nil {
			killAndWait(proc)
		}
		tn.lastErr = errRemoved
		return
	}
	tn.lastErr = err
	if err != nil {
		return
	}
	tn.proc, tn.localPort, tn.remote = proc, port, disc
	flog.Info("ssh tunnel up", "remote", t.String(), "local", loopback(port), "remotePort", disc.Port)
}

// establish runs one bring-up: discovery, then the forward on localPort (or a
// fresh port when 0 / taken), readiness, and a verifying Hello — retried on a
// fresh local port when ssh could not bind the chosen one. The port is checked
// free BEFORE spawning (a squatter would otherwise accept our readiness probe
// and ssh's bind failure would go unnoticed until the Hello), and a failed
// Hello re-checks ssh's stderr for the same reason — a squatter that arrived
// during ssh's connect+auth window.
func (m *Manager) establish(t Target, localPort int) (forwardProc, int, Discovery, error) {
	ctx := m.ctx
	disc, err := m.discover(ctx, t)
	if err != nil {
		return nil, localPort, Discovery{}, fmt.Errorf("%s: %w", t.Host, err)
	}
	for attempt := 0; ; attempt++ {
		if localPort == 0 || !portFree(localPort) {
			port, err := freePort()
			if err != nil {
				return nil, localPort, Discovery{}, fmt.Errorf("pick local port: %w", err)
			}
			localPort = port
		}
		addr := loopback(localPort)
		proc, err := m.forward(ctx, t, localPort, disc.Port)
		if err != nil {
			return nil, localPort, Discovery{}, err
		}
		err = waitForwardReady(ctx, proc, localPort)
		if err != nil {
			// The forward's own ssh can refuse the host key too (known_hosts
			// edited between discovery and now): classify it the same way.
			if hkErr := hostKeyError(ctx, t, proc.Stderr()); hkErr != nil {
				err = hkErr
			}
		}
		if err == nil {
			hctx, cancel := context.WithTimeout(ctx, helloTimeout)
			err = m.hello(hctx, addr, disc.Token)
			cancel()
			if err != nil {
				err = fmt.Errorf("tunnel up but the remote daemon did not answer: %w", err)
			}
		}
		if err == nil {
			return proc, localPort, disc, nil
		}
		killAndWait(proc)
		if attempt < 2 && (isBindFailure(err.Error()) || isBindFailure(proc.Err())) {
			flog.Warn("ssh tunnel local port taken; retrying on another", "remote", t.String(), "port", localPort)
			localPort = 0
			continue
		}
		return nil, localPort, Discovery{}, err
	}
}

// recordOffer remembers the known_hosts lines an UnknownHostKeyError carries,
// so TrustHostKey can later accept exactly one of them and nothing else.
func (m *Manager) recordOffer(t Target, err error) {
	var uk *UnknownHostKeyError
	if !errors.As(err, &uk) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lines := make(map[string]string, len(uk.Keys))
	for _, k := range uk.Keys {
		lines[k.Line] = uk.KnownHostsPath
	}
	m.offered[t.String()] = lines
}

// ErrKeyNotOffered is returned by TrustHostKey for a line the daemon never
// offered for that remote (stale prompt, or a caller inventing lines).
var ErrKeyNotOffered = errors.New("that host key was not offered for this remote — run the connection test again")

// TrustHostKey appends one previously offered known_hosts line for rawURL to
// the daemon user's known_hosts (see appendKnownHosts) and then resolves the
// remote, returning its endpoint. The offer is consumed: a second call with
// the same line is refused, so a key is written once, on one explicit accept.
func (m *Manager) TrustHostKey(ctx context.Context, rawURL, line string) (Endpoint, error) {
	t, err := ParseURL(rawURL)
	if err != nil {
		return Endpoint{}, err
	}
	m.mu.Lock()
	path, ok := m.offered[t.String()][line]
	m.mu.Unlock()
	if !ok {
		return Endpoint{}, ErrKeyNotOffered
	}
	if err := appendKnownHosts(path, line); err != nil {
		// The offer stands: a failed write (permissions, disk) can be retried
		// from the same prompt without another connection test.
		return Endpoint{}, fmt.Errorf("write %s: %w", path, err)
	}
	m.mu.Lock()
	delete(m.offered, t.String()) // consumed: one accept writes one line, once
	m.mu.Unlock()
	flog.Info("ssh host key trusted", "remote", t.String(), "knownHosts", path, "line", line)
	return m.Resolve(ctx, rawURL)
}

// alive reports whether a forward process is still running.
func alive(proc forwardProc) bool {
	select {
	case <-proc.Done():
		return false
	default:
		return true
	}
}

// killAndWait tears a forward down and waits (bounded) for it to exit, so its
// local port is released before the caller checks or reuses it and its stderr
// diagnostic is complete.
func killAndWait(proc forwardProc) {
	proc.Kill()
	select {
	case <-proc.Done():
	case <-time.After(killWait):
	}
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

// Key returns the canonical form of an ssh:// URL — the identity Remove
// matches on — or "" for anything that is not a valid ssh:// URL.
func Key(raw string) string {
	t, err := ParseURL(raw)
	if err != nil {
		return ""
	}
	return t.String()
}

// Remove tears down the forwards of the given remotes (URLs that don't parse
// are ignored). Only remotes the user actually dropped should be passed — a
// tunnel is never pruned merely for being unregistered, since an env-booted
// FLEET_SSH session rides one too. A remote mid-bring-up is marked removed and
// its bring-up discards the forward when it finishes; nothing here waits on a
// bring-up, so the caller (SetArmada under muWrite) is never stalled by one.
func (m *Manager) Remove(urls []string) {
	for _, raw := range urls {
		key := Key(raw)
		if key == "" {
			continue
		}
		m.mu.Lock()
		tn, ok := m.tunnels[key]
		delete(m.tunnels, key)
		m.mu.Unlock()
		if !ok {
			continue
		}
		tn.mu.Lock()
		tn.removed = true
		if tn.proc != nil {
			killAndWait(tn.proc)
			tn.proc = nil
		}
		tn.mu.Unlock()
		flog.Info("ssh tunnel removed", "remote", key)
	}
}

// Close kills every forward (daemon shutdown).
func (m *Manager) Close() {
	m.mu.Lock()
	keys := make([]string, 0, len(m.tunnels))
	for key := range m.tunnels {
		keys = append(keys, key)
	}
	m.mu.Unlock()
	m.Remove(keys)
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
// The caller bounds ctx.
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
	_, err = fleetgrpc.NewFleetServiceClient(cc).Hello(ctx, &fleetgrpc.HelloRequest{ClientVersion: version.Version})
	return err
}
