package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/agentfwd"
	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
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
}

func attachRawProvider(t *testing.T, client fleetgrpc.FleetServiceClient, name string) *rawProvider {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.SSHAgent(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := stream.Send(&fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Hello{Hello: &fleetgrpc.SSHAgentHello{Client: name}}}); err != nil {
		cancel()
		t.Fatal(err)
	}
	p := &rawProvider{t: t, stream: stream, downs: make(chan *fleetgrpc.SSHAgentDown, 64), cancel: cancel}
	go func() {
		defer close(p.downs)
		for {
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

func TestSSHAgentInstanceSocketsFollowState(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, client := startAgentTestServer(t)

	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"f": {Name: "f", Instances: []*fleet.Instance{
			{Name: "i", Backend: fleet.BackendDevcontainer, Status: fleet.StatusRunning},
			{Name: "nocontrol", Backend: fleet.BackendCoder, Status: fleet.StatusRunning},
		}},
	}}
	if err := os.MkdirAll(state.ControlDir("f", "i"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc.agent.syncInstances(st)

	sock := filepath.Join(state.ControlDir("f", "i"), agentsock.SocketName)
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("instance socket not created: %v", err)
	}
	// 0666 so a container user with another uid can connect; the peer check
	// (daemon user, root, or that instance's container) decides who is served.
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o666 {
		t.Fatalf("instance socket mode = %v, want a 0666 socket", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(state.ControlDir("f", "nocontrol"), agentsock.SocketName)); err == nil {
		t.Fatal("an instance without a control directory must not get a socket")
	}

	provider := attachRawProvider(t, client, "laptop")
	provider.nextStatus()
	dialAsync(t, sock)
	if origin := provider.nextOpen().GetOrigin(); origin != "f/i" {
		t.Fatalf("origin = %q, want f/i", origin)
	}

	// The instance is destroyed: its socket goes with it.
	svc.agent.syncInstances(&state.State{Fleets: map[string]*fleet.Fleet{}})
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket should be removed with its instance, stat err = %v", err)
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

	done := dialAsync(t, hostSock)
	open := provider.nextOpen()
	if err := provider.stream.Send(&fleetgrpc.SSHAgentUp{Msg: &fleetgrpc.SSHAgentUp_Ready{Ready: &fleetgrpc.SSHAgentReady{ConnId: open.GetConnId()}}}); err != nil {
		t.Fatal(err)
	}

	svc.agent.close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the hub left a relayed connection open")
	}
	if _, err := os.Stat(hostSock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closing the hub should remove the host socket, stat err = %v", err)
	}
}

func TestSSHAgentInstanceSocketWithLongNames(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, client := startAgentTestServer(t)

	// Far past the 108-byte unix socket path limit.
	fleetName := "a-rather-long-fleet-name-for-the-platform-backend-service"
	instName := "feature-auth-refactor-with-a-long-descriptive-instance-name"
	control := state.ControlDir(fleetName, instName)
	if len(control) < 110 {
		t.Fatalf("test setup: control dir %d bytes, want it past the socket limit", len(control))
	}
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	svc.agent.ensureInstance(fleetName, instName)
	sock := filepath.Join(control, agentsock.SocketName)
	if info, err := os.Lstat(sock); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("no socket at a long path: %v", err)
	}

	provider := attachRawProvider(t, client, "laptop")
	provider.nextStatus()
	// Connect the way a container does, through a short path to the same
	// directory (a symlink standing in for the /fleet-mounts/control mount).
	short := filepath.Join(dir, "m")
	if err := os.Symlink(control, short); err != nil {
		t.Fatal(err)
	}
	dialAsync(t, filepath.Join(short, agentsock.SocketName))
	if origin := provider.nextOpen().GetOrigin(); origin != fleetName+"/"+instName {
		t.Fatalf("origin = %q", origin)
	}
}

func TestSSHAgentInstanceSocketNeverFollowsASymlink(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, _ := startAgentTestServer(t)

	control := state.ControlDir("f", "i")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	// A process in the instance plants a symlink where the socket goes,
	// pointing at a host file it wants the daemon to chmod.
	target := filepath.Join(dir, "precious")
	if err := os.WriteFile(target, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(control, agentsock.SocketName)); err != nil {
		t.Fatal(err)
	}
	svc.agent.ensureInstance("f", "i")
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("the symlink target's mode changed: %v %v", info.Mode(), err)
	}
	if info, err := os.Lstat(filepath.Join(control, agentsock.SocketName)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the planted entry should be left alone (and no socket served): %v", err)
	}
}

func TestSSHAgentInstanceSocketIsRecreatedWhenDeleted(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("HOME", dir)
	svc, _ := startAgentTestServer(t)
	st := &state.State{Fleets: map[string]*fleet.Fleet{
		"f": {Name: "f", Instances: []*fleet.Instance{{Name: "i", Backend: fleet.BackendDevcontainer}}},
	}}
	control := state.ControlDir("f", "i")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	svc.agent.syncInstances(st)
	sock := filepath.Join(control, agentsock.SocketName)
	// The instance is destroyed and re-created under the same name between
	// two reconciles: the old listener is bound to nothing.
	if err := os.RemoveAll(control); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	svc.agent.syncInstances(st) // notices the stale listener
	svc.agent.syncInstances(st) // opens a fresh one
	if info, err := os.Lstat(sock); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket not recreated: %v", err)
	}
}
