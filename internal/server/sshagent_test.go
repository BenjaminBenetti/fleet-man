package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentfwd"
	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/create"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// sshagent_test.go drives the relay end to end: a real gRPC server with the
// service's SSHAgent handler, real unix sockets, and in-process ssh-agents
// (x/crypto/ssh/agent keyrings) standing in for the user's and the daemon's.

// shortTempDir is a temp dir short enough for unix socket paths (t.TempDir()
// with the test name appended can exceed the 104/108-byte sun_path limit).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startFakeAgent serves an agent holding one fresh ed25519 key at a socket
// under dir and returns the socket and the key.
func startFakeAgent(t *testing.T, dir, name string) (string, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv, Comment: name}); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, name+".sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = agent.ServeAgent(keyring, conn)
			}()
		}
	}()
	return sock, signer.PublicKey()
}

// startAgentTestServer serves a fresh service over loopback gRPC and returns
// it with a client.
func startAgentTestServer(t *testing.T) (*service, fleetgrpc.FleetServiceClient) {
	t.Helper()
	t.Setenv(agentsock.EnvOverride, "")
	svc := newService()
	gs := grpc.NewServer()
	fleetgrpc.RegisterFleetServiceServer(gs, svc)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(ln) }()
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		gs.Stop()
		svc.agent.close()
	})
	return svc, fleetgrpc.NewFleetServiceClient(conn)
}

// agentKeys lists the keys an agent client sees through sock (an error when
// the relay closed the connection unserved).
func agentKeys(sock string) ([]*agent.Key, error) {
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	return agent.NewClient(conn).List()
}

func requireOnlyKey(t *testing.T, sock string, want ssh.PublicKey) {
	t.Helper()
	keys, err := agentKeys(sock)
	if err != nil {
		t.Fatalf("list keys through %s: %v", sock, err)
	}
	if len(keys) != 1 || string(keys[0].Marshal()) != string(want.Marshal()) {
		t.Fatalf("keys through relay = %v, want exactly %s", keys, ssh.FingerprintSHA256(want))
	}
}

// statusRecorder collects provider reports.
type statusRecorder chan agentfwd.Status

func (r statusRecorder) report(st agentfwd.Status) {
	select {
	case r <- st:
	default:
	}
}

func (r statusRecorder) waitFor(t *testing.T, want agentfwd.State) agentfwd.Status {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case st := <-r:
			if st.State == want {
				return st
			}
		case <-deadline:
			t.Fatalf("provider never reached state %v", want)
		}
	}
}

func TestSSHAgentRelayServesTheProvidersAgent(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	userSock, userKey := startFakeAgent(t, dir, "user")
	t.Setenv("SSH_AUTH_SOCK", userSock)

	svc, client := startAgentTestServer(t)
	hostSock := filepath.Join(dir, "relay.sock")
	if err := svc.agent.listenHost(hostSock); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	statuses := make(statusRecorder, 64)
	go agentfwd.Run(ctx, client, "test", statuses.report)
	statuses.waitFor(t, agentfwd.StateActive)

	requireOnlyKey(t, hostSock, userKey)

	// A signature made through the relay verifies against the user's key: the
	// bytes really went to the user's agent and back.
	conn, err := net.Dial("unix", hostSock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	data := []byte("git push")
	sig, err := agent.NewClient(conn).Sign(userKey, data)
	if err != nil {
		t.Fatalf("sign through relay: %v", err)
	}
	if err := userKey.Verify(data, sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

func TestSSHAgentRelayFallsBackToTheDaemonsAgent(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	daemonSock, daemonKey := startFakeAgent(t, dir, "daemon")

	svc, _ := startAgentTestServer(t)
	svc.agent.setFallback(daemonSock)
	hostSock := filepath.Join(dir, "relay.sock")
	if err := svc.agent.listenHost(hostSock); err != nil {
		t.Fatal(err)
	}
	requireOnlyKey(t, hostSock, daemonKey)
}

func TestSSHAgentRelayWithNoUpstreamClosesTheConnection(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, _ := startAgentTestServer(t)
	hostSock := filepath.Join(dir, "relay.sock")
	if err := svc.agent.listenHost(hostSock); err != nil {
		t.Fatal(err)
	}
	if keys, err := agentKeys(hostSock); err == nil {
		t.Fatalf("expected the unserved connection to be closed, got keys %v", keys)
	}
}

// rawProvider is a hand-driven SSHAgent stream, for routing tests.
type rawProvider struct {
	t      *testing.T
	stream fleetgrpc.FleetService_SSHAgentClient
	downs  chan *fleetgrpc.SSHAgentDown
	cancel context.CancelFunc
	paused chan struct{} // closed: stop reading the stream
}

// pause stops reading the stream altogether (the provider stops draining).
func (p *rawProvider) pause() { close(p.paused) }

func attachRawProvider(t *testing.T, client fleetgrpc.FleetServiceClient, name string) *rawProvider {
	t.Helper()
	return attachRawProviderAs(t, client, name, false)
}

func attachRawProviderAs(t *testing.T, client fleetgrpc.FleetServiceClient, name string, yield bool) *rawProvider {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.SSHAgent(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := stream.Send(&fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Hello{Hello: &fleetgrpc.SSHAgentHello{Client: name, Yield: yield}}}); err != nil {
		cancel()
		t.Fatal(err)
	}
	p := &rawProvider{t: t, stream: stream, downs: make(chan *fleetgrpc.SSHAgentDown, 64), cancel: cancel, paused: make(chan struct{})}
	go func() {
		defer close(p.downs)
		for {
			select {
			case <-p.paused:
				<-ctx.Done()
				return
			default:
			}
			down, err := stream.Recv()
			if err != nil {
				return
			}
			p.downs <- down
		}
	}()
	t.Cleanup(cancel)
	return p
}

// next returns the next frame of the wanted kind, skipping others.
func (p *rawProvider) nextStatus() *fleetgrpc.SSHAgentStatus {
	p.t.Helper()
	for {
		select {
		case down, ok := <-p.downs:
			if !ok {
				p.t.Fatal("provider stream ended")
			}
			if st := down.GetStatus(); st != nil {
				return st
			}
		case <-time.After(10 * time.Second):
			p.t.Fatal("no status frame")
		}
	}
}

func (p *rawProvider) nextOpen() *fleetgrpc.SSHAgentOpen {
	p.t.Helper()
	for {
		select {
		case down, ok := <-p.downs:
			if !ok {
				p.t.Fatal("provider stream ended")
			}
			if open := down.GetOpen(); open != nil {
				return open
			}
		case <-time.After(10 * time.Second):
			p.t.Fatal("no open frame")
		}
	}
}

// noOpen asserts no connection is announced to p for a moment.
func (p *rawProvider) noOpen() {
	p.t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case down := <-p.downs:
			if down.GetOpen() != nil {
				p.t.Fatalf("unexpected open on a provider that should not serve: %v", down)
			}
		case <-deadline:
			return
		}
	}
}

func (p *rawProvider) refuse(connID uint64) {
	p.t.Helper()
	if err := p.stream.Send(&fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Close{Close: &fleetgrpc.SSHAgentClose{ConnId: connID, Error: "no agent here"}}}); err != nil {
		p.t.Fatal(err)
	}
}

// dialAsync opens a connection to sock and keeps it until the test ends,
// reading until the relay closes it.
func dialAsync(t *testing.T, sock string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		_, err := io.Copy(io.Discard, conn)
		done <- err
	}()
	return done
}

func TestSSHAgentRelayNewestProviderWinsThenTheOlderIsPromoted(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, client := startAgentTestServer(t)
	hostSock := filepath.Join(dir, "relay.sock")
	if err := svc.agent.listenHost(hostSock); err != nil {
		t.Fatal(err)
	}

	older := attachRawProvider(t, client, "laptop")
	if !older.nextStatus().GetActive() {
		t.Fatal("first provider should be active")
	}
	newer := attachRawProvider(t, client, "desktop")
	if !newer.nextStatus().GetActive() {
		t.Fatal("newest provider should be active")
	}
	if older.nextStatus().GetActive() {
		t.Fatal("the superseded provider should be told it is standing by")
	}

	dialAsync(t, hostSock)
	newer.nextOpen()
	older.noOpen()

	newer.cancel()
	if !older.nextStatus().GetActive() {
		t.Fatal("the older provider should be promoted when the newer leaves")
	}
	dialAsync(t, hostSock)
	older.nextOpen()
}

func TestSSHAgentRelayProviderThatCannotServeFallsThrough(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	daemonSock, daemonKey := startFakeAgent(t, dir, "daemon")
	svc, client := startAgentTestServer(t)
	svc.agent.setFallback(daemonSock)
	hostSock := filepath.Join(dir, "relay.sock")
	if err := svc.agent.listenHost(hostSock); err != nil {
		t.Fatal(err)
	}

	older := attachRawProvider(t, client, "laptop")
	older.nextStatus()
	newer := attachRawProvider(t, client, "desktop")
	newer.nextStatus()

	type listResult struct {
		keys []*agent.Key
		err  error
	}
	result := make(chan listResult, 1)
	go func() {
		keys, err := agentKeys(hostSock)
		result <- listResult{keys, err}
	}()

	// The active provider has no agent: the next-newest gets the connection;
	// when it cannot serve either, the daemon's own agent does.
	newer.refuse(newer.nextOpen().GetConnId())
	older.refuse(older.nextOpen().GetConnId())

	res := <-result
	if res.err != nil {
		t.Fatalf("list keys: %v", res.err)
	}
	if len(res.keys) != 1 || string(res.keys[0].Marshal()) != string(daemonKey.Marshal()) {
		t.Fatalf("expected the daemon's key after both providers refused, got %v", res.keys)
	}
}

func TestSSHAgentRefusedWhenForwardingIsOff(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	_, client := startAgentTestServer(t)
	t.Setenv(agentsock.EnvOverride, "off")

	stream, err := client.SSHAgent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(&fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Hello{Hello: &fleetgrpc.SSHAgentHello{Client: "x"}}})
	if _, err := stream.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition with forwarding off, got %v", err)
	}
}

func TestListenAgentSocketReplacesOnlyAStaleSocket(t *testing.T) {
	dir := shortTempDir(t)

	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false) // leave the file behind, like a crashed daemon
	}
	_ = ln.Close()
	l, err := listenAgentSocket(stale, func(c net.Conn) { _ = c.Close() })
	if err != nil {
		t.Fatalf("a stale socket should be replaced: %v", err)
	}
	l.Close()

	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenAgentSocket(regular, func(c net.Conn) { _ = c.Close() }); err == nil {
		t.Fatal("a non-socket file must not be replaced")
	}
	if data, _ := os.ReadFile(regular); string(data) != "keep me" {
		t.Fatal("the non-socket file was modified")
	}
}

func TestSSHAgentHubCloseEndsLiveConnections(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, client := startAgentTestServer(t)
	hostSock := filepath.Join(dir, "relay.sock")
	if err := svc.agent.listenHost(hostSock); err != nil {
		t.Fatal(err)
	}
	provider := attachRawProvider(t, client, "laptop")
	provider.nextStatus()

	conn, err := net.Dial("unix", hostSock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	open := provider.nextOpen()
	if err := provider.stream.Send(&fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Ready{Ready: &fleetgrpc.SSHAgentReady{ConnId: open.GetConnId()}}}); err != nil {
		t.Fatal(err)
	}
	// Prove the relay is LIVE (past ready): bytes written now reach the
	// provider as data for this connection.
	if _, err := conn.Write([]byte{0, 0, 0, 1, 11}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for live := false; !live; {
		select {
		case down := <-provider.downs:
			live = down.GetData().GetConnId() == open.GetConnId()
		case <-deadline:
			t.Fatal("the relay never went live")
		}
	}

	readDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, conn)
		close(readDone)
	}()
	closed := make(chan struct{})
	go func() {
		svc.agent.close()
		close(closed)
	}()
	for _, ch := range []chan struct{}{readDone, closed} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("closing the hub left a live relayed connection open (or hung)")
		}
	}
	if _, err := os.Stat(hostSock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closing the hub should remove the host socket, stat err = %v", err)
	}
}

// halfCloseList sends REQUEST_IDENTITIES, shuts the write side at once (as
// socat or `nc -N` do), and returns everything read back.
func halfCloseList(t *testing.T, sock string) []byte {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := conn.Write([]byte{0, 0, 0, 1, 11}); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return reply
}

// isOneKeyAnswer reports whether reply is an identities answer with one key.
func isOneKeyAnswer(reply []byte) bool {
	return len(reply) >= 9 && reply[4] == 12 && reply[5] == 0 && reply[6] == 0 && reply[7] == 0 && reply[8] == 1
}

func TestSSHAgentRelayHalfCloseStillGetsTheReply(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, client := startAgentTestServer(t)
	hostSock := filepath.Join(dir, "relay.sock")
	if err := svc.agent.listenHost(hostSock); err != nil {
		t.Fatal(err)
	}

	t.Run("fallback", func(t *testing.T) {
		daemonSock, _ := startFakeAgent(t, dir, "daemon")
		svc.agent.setFallback(daemonSock)
		t.Cleanup(func() { svc.agent.setFallback("") })
		if reply := halfCloseList(t, hostSock); !isOneKeyAnswer(reply) {
			t.Fatalf("reply through the fallback after a half-close = %v", reply)
		}
	})

	t.Run("provider", func(t *testing.T) {
		userSock, _ := startFakeAgent(t, dir, "user")
		t.Setenv("SSH_AUTH_SOCK", userSock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		statuses := make(statusRecorder, 64)
		go agentfwd.Run(ctx, client, "test", statuses.report)
		statuses.waitFor(t, agentfwd.StateActive)
		if reply := halfCloseList(t, hostSock); !isOneKeyAnswer(reply) {
			t.Fatalf("reply through the provider after a half-close = %v", reply)
		}
	})
}

func TestSSHAgentSilentProviderIsSkipped(t *testing.T) {
	orig := agentPingInterval
	agentPingInterval = 50 * time.Millisecond
	t.Cleanup(func() { agentPingInterval = orig })

	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	daemonSock, daemonKey := startFakeAgent(t, dir, "daemon")
	svc, client := startAgentTestServer(t)
	svc.agent.setFallback(daemonSock)
	hostSock := filepath.Join(dir, "relay.sock")
	if err := svc.agent.listenHost(hostSock); err != nil {
		t.Fatal(err)
	}

	// A provider that never answers pings (a laptop gone to sleep).
	sleeper := attachRawProvider(t, client, "laptop")
	sleeper.nextStatus()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p := svc.agent.providersNewestFirst()
		if len(p) == 1 && p[0].isSuspect() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the silent provider was never marked suspect")
		}
		time.Sleep(20 * time.Millisecond)
	}

	start := time.Now()
	requireOnlyKey(t, hostSock, daemonKey)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a suspect provider still stalled the connection for %s", elapsed)
	}

	// It wakes up and answers: it is tried again.
	if err := sleeper.stream.Send(&fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Pong{Pong: &fleetgrpc.SSHAgentPong{Seq: 1 << 30}}}); err != nil {
		t.Fatal(err)
	}
	wake := time.Now().Add(5 * time.Second)
	for {
		if p := svc.agent.providersNewestFirst(); len(p) == 1 && !p[0].isSuspect() {
			break
		}
		if time.Now().After(wake) {
			t.Fatal("the provider stayed suspect after it answered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	dialAsync(t, hostSock)
	sleeper.nextOpen()
}

func TestStartAgentRelayRedirectsTheDaemonsAgent(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	daemonSock, daemonKey := startFakeAgent(t, dir, "daemon")
	// t.Setenv first so its cleanup also undoes startAgentRelay's os.Setenv.
	t.Setenv(agentsock.EnvAuthSock, daemonSock)
	t.Setenv(agentsock.EnvOrigin, "")
	_ = os.Unsetenv(agentsock.EnvOrigin)
	t.Setenv(agentsock.EnvOverride, "")
	origHook := create.ControlDirReady
	t.Cleanup(func() {
		create.ControlDirReady = origHook
		agentsock.SetRelayServing(false)
	})

	h := newAgentHub()
	ctx, cancel := context.WithCancel(context.Background())
	startAgentRelay(ctx, h)
	t.Cleanup(func() {
		cancel()
		h.close()
	})

	if got := os.Getenv(agentsock.EnvAuthSock); got != agentsock.HostSocketPath() {
		t.Fatalf("SSH_AUTH_SOCK = %q, want the relay %q", got, agentsock.HostSocketPath())
	}
	if got := os.Getenv(agentsock.EnvOrigin); got != daemonSock {
		t.Fatalf("%s = %q, want the original agent", agentsock.EnvOrigin, got)
	}
	if agentsock.OriginSock() != daemonSock {
		t.Fatal("OriginSock must see through the redirect")
	}
	if !agentsock.RelayUsable() {
		t.Fatal("the relay should be usable: the daemon has an agent to fall back to")
	}
	// Provisioning's hook opens an instance's socket at once, before any
	// reconcile could (`devcontainer up` may run postCreate within seconds).
	if !agentInstanceSocketsSupported {
		return
	}
	if err := os.MkdirAll(state.ControlDir("f", "i"), 0o755); err != nil {
		t.Fatal(err)
	}
	create.ControlDirReady("f", "i")
	if info, err := os.Lstat(filepath.Join(state.ControlDir("f", "i"), agentsock.SocketName)); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("the provisioning hook did not open the instance socket: %v", err)
	}
	// A child of the daemon (git clone) reaches the original agent through it.
	requireOnlyKey(t, os.Getenv(agentsock.EnvAuthSock), daemonKey)
}

func TestStartAgentRelayLeavesTheEnvironmentAloneWhenOff(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	t.Setenv(agentsock.EnvAuthSock, "/tmp/some-agent.sock")
	t.Setenv(agentsock.EnvOverride, "off")
	origHook := create.ControlDirReady
	t.Cleanup(func() { create.ControlDirReady = origHook })

	h := newAgentHub()
	ctx, cancel := context.WithCancel(context.Background())
	startAgentRelay(ctx, h)
	t.Cleanup(func() {
		cancel()
		h.close()
	})
	if got := os.Getenv(agentsock.EnvAuthSock); got != "/tmp/some-agent.sock" {
		t.Fatalf("SSH_AUTH_SOCK changed to %q with forwarding off", got)
	}
	if _, err := os.Stat(agentsock.HostSocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no relay socket may exist with forwarding off: %v", err)
	}
}

func TestSSHAgentYieldingProviderQueuesBehindTheTUI(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, client := startAgentTestServer(t)
	hostSock := filepath.Join(dir, "relay.sock")
	if err := svc.agent.listenHost(hostSock); err != nil {
		t.Fatal(err)
	}

	tui := attachRawProviderAs(t, client, "laptop (tui)", false)
	if !tui.nextStatus().GetActive() {
		t.Fatal("the TUI should be active")
	}
	// A `fleet shell` the TUI spawned attaches later but yields.
	shell := attachRawProviderAs(t, client, "laptop (cli)", true)
	if shell.nextStatus().GetActive() {
		t.Fatal("a yielding provider must not become active over the TUI")
	}
	dialAsync(t, hostSock)
	tui.nextOpen()
	shell.noOpen()

	// A second TUI (another machine) still supersedes normally.
	desk := attachRawProviderAs(t, client, "desktop (tui)", false)
	if !desk.nextStatus().GetActive() || tui.nextStatus().GetActive() {
		t.Fatal("a newer non-yielding provider takes over")
	}

	// With every non-yielding provider gone, the yielding one serves.
	desk.cancel()
	tui.cancel()
	if !shell.nextStatus().GetActive() {
		t.Fatal("the yielding provider should be promoted when nothing else is left")
	}
	dialAsync(t, hostSock)
	shell.nextOpen()
}

func TestSSHAgentStalledProviderFailsOver(t *testing.T) {
	orig := agentReadyTimeout
	agentReadyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { agentReadyTimeout = orig })

	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	daemonSock, daemonKey := startFakeAgent(t, dir, "daemon")
	svc, client := startAgentTestServer(t)
	svc.agent.setFallback(daemonSock)
	hostSock := filepath.Join(dir, "relay.sock")
	if err := svc.agent.listenHost(hostSock); err != nil {
		t.Fatal(err)
	}

	// A provider that answers one open and then stops reading its stream
	// entirely (Ctrl-Z on a `fleet up`, a laptop asleep behind a live TCP).
	stalled := attachRawProvider(t, client, "laptop")
	stalled.nextStatus()
	flood, err := net.Dial("unix", hostSock)
	if err != nil {
		t.Fatal(err)
	}
	defer flood.Close()
	open := stalled.nextOpen()
	if err := stalled.stream.Send(&fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Ready{Ready: &fleetgrpc.SSHAgentReady{ConnId: open.GetConnId()}}}); err != nil {
		t.Fatal(err)
	}
	// Stop draining: from here on nothing reads the provider's downs.
	stalled.pause()
	go func() {
		chunk := make([]byte, 64*1024)
		for i := 0; i < 256; i++ { // 16 MiB: far past every window and queue
			if _, err := flood.Write(chunk); err != nil {
				return
			}
		}
	}()
	time.Sleep(time.Second) // let the stream back up

	// Another connection must not hang behind the stalled stream: it fails
	// over to the daemon's own agent within the ready timeout.
	start := time.Now()
	requireOnlyKey(t, hostSock, daemonKey)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("a stalled provider held a new connection for %s", elapsed)
	}
}

// realOpenSSHAgent starts OpenSSH's ssh-agent holding one fresh key; skips
// when it is not installed.
func realOpenSSHAgent(t *testing.T, dir string) string {
	t.Helper()
	for _, tool := range []string{"ssh-agent", "ssh-add", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	sock := filepath.Join(dir, "openssh.sock")
	cmd := exec.Command("ssh-agent", "-D", "-a", sock)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	key := filepath.Join(dir, "k")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	add := exec.Command("ssh-add", "-q", key)
	add.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("ssh-add: %v\n%s", err, out)
	}
	return sock
}

func TestSSHAgentFallbackHalfCloseWithARealOpenSSHAgent(t *testing.T) {
	// x/crypto's agent answers before it notices EOF; OpenSSH's drops the
	// connection on EOF without answering — the relay must not pass a
	// client's half-close on before the reply is in.
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	sock := realOpenSSHAgent(t, dir)
	svc, _ := startAgentTestServer(t)
	svc.agent.setFallback(sock)
	hostSock := filepath.Join(dir, "relay.sock")
	if err := svc.agent.listenHost(hostSock); err != nil {
		t.Fatal(err)
	}
	if reply := halfCloseList(t, hostSock); !isOneKeyAnswer(reply) {
		t.Fatalf("host socket, half-close: %v", reply)
	}
	if !agentInstanceSocketsSupported {
		return
	}
	control := state.ControlDir("f", "i")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	svc.agent.ensureInstance("f", "i")
	if reply := halfCloseList(t, filepath.Join(control, agentsock.SocketName)); !isOneKeyAnswer(reply) {
		t.Fatalf("instance socket, half-close: %v", reply)
	}
}

func TestStartAgentRelayWaitsForSomethingToAnswer(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	// A headless remote host: started without an agent, nobody has forwarded
	// one to it yet.
	t.Setenv(agentsock.EnvAuthSock, "")
	t.Setenv(agentsock.EnvOrigin, "")
	_ = os.Unsetenv(agentsock.EnvOrigin)
	svc, client := startAgentTestServer(t)
	origHook := create.ControlDirReady
	t.Cleanup(func() {
		create.ControlDirReady = origHook
		agentsock.SetRelayServing(false)
		agentsock.SetRemoteClients(false)
		agentsock.SetProviderSeen(false)
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startAgentRelay(ctx, svc.agent)
	agentsock.SetRemoteClients(true) // Remote Fleet is on

	if got := os.Getenv(agentsock.EnvAuthSock); got != "" {
		t.Fatalf("SSH_AUTH_SOCK = %q before anything can answer on the relay; host scripts must see it unset", got)
	}
	if agentsock.RelayUsable() {
		t.Fatal("instances must not be pointed at a relay nothing can answer")
	}

	p := attachRawProvider(t, client, "laptop")
	p.nextStatus()
	if got := os.Getenv(agentsock.EnvAuthSock); got != agentsock.HostSocketPath() {
		t.Fatalf("after the first provider, SSH_AUTH_SOCK = %q, want the relay", got)
	}
	if _, err := os.Stat(agentsock.ProviderSeenPath()); err != nil {
		t.Fatalf("the first provider must be remembered across restarts: %v", err)
	}
	if !agentsock.RelayUsable() {
		t.Fatal("with a provider seen and Remote Fleet on, the relay is worth pointing instances at")
	}
}
