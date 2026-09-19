package sshtunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// e2e_test.go drives the PRODUCTION discovery + forward code through the real
// `ssh` binary against a tiny in-process SSH server (x/crypto/ssh) that speaks
// just enough of the protocol: public-key auth, an "exec" session (the
// discovery script under sh), and "direct-tcpip" channels (the -L forward).
// The "remote" ~/.fleet is a temp dir whose ssh.port points at an in-process
// token-gated gRPC server, so the whole chain — script over ssh, port-forward,
// bearer-token Hello — is exercised end to end without a system sshd.

// testSSHServer is the minimal server. remoteHome is the HOME the exec'd
// command sees (the fake remote's ~/.fleet lives under it); remoteBin is put
// first on its PATH and holds a fake `fleet` (the probe's liveness oracle) so
// a real fleet binary on this machine can never be run against the temp HOME.
type testSSHServer struct {
	addr       string
	hostPub    ssh.PublicKey
	remoteHome string
	remoteBin  string
	stop       func()
}

func startTestSSHServer(t *testing.T, clientPub ssh.PublicKey, remoteHome string) *testSSHServer {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), clientPub.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &testSSHServer{addr: ln.Addr().String(), hostPub: hostSigner.PublicKey(), remoteHome: remoteHome, remoteBin: fakeFleet(t, "exit 0\n")}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	srv.stop = func() {
		cancel()
		_ = ln.Close()
		wg.Wait()
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				srv.serveConn(ctx, c, cfg)
			}()
		}
	}()
	t.Cleanup(srv.stop)
	return srv
}

func (s *testSSHServer) serveConn(ctx context.Context, c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	context.AfterFunc(ctx, func() { _ = conn.Close() })
	for newCh := range chans {
		switch newCh.ChannelType() {
		case "session":
			go s.handleSession(newCh)
		case "direct-tcpip":
			go handleDirectTCPIP(newCh)
		default:
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

// handleSession runs the "exec" command (the discovery probe runs `sh` with
// the script on stdin) under the fake remote's HOME.
func (s *testSSHServer) handleSession(newCh ssh.NewChannel) {
	ch, reqs, err := newCh.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			cmd := exec.Command("sh", "-c", payload.Command)
			cmd.Env = []string{"HOME=" + s.remoteHome, "PATH=" + s.remoteBin + ":/usr/bin:/bin"}
			cmd.Stdin = ch
			cmd.Stdout = ch
			cmd.Stderr = ch.Stderr()
			status := uint32(0)
			if err := cmd.Run(); err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					status = uint32(exitErr.ExitCode())
				} else {
					status = 255
				}
			}
			var buf [4]byte
			binary.BigEndian.PutUint32(buf[:], status)
			_, _ = ch.SendRequest("exit-status", false, buf[:])
			return
		default:
			_ = req.Reply(false, nil)
		}
	}
}

// handleDirectTCPIP is the -L forward: dial what the client asked for and pipe.
func handleDirectTCPIP(newCh ssh.NewChannel) {
	var payload struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	if err := ssh.Unmarshal(newCh.ExtraData(), &payload); err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	target, err := net.DialTimeout("tcp", net.JoinHostPort(payload.Host, strconv.Itoa(int(payload.Port))), 3*time.Second)
	if err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := newCh.Accept()
	if err != nil {
		_ = target.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(target, ch); done <- struct{}{} }()
	go func() { _, _ = io.Copy(ch, target); done <- struct{}{} }()
	<-done
	_ = ch.Close()
	_ = target.Close()
}

// clientSSHHome builds the ssh CLIENT side (the daemon's): a key pair, a
// known_hosts entry for the test server, and a config that pins them for
// 127.0.0.1. finish installs that config via the -F seam (OpenSSH ignores
// $HOME for ~/.ssh), restoring the production args when the test ends.
func clientSSHHome(t *testing.T) (home string, pub ssh.PublicKey, finish func(srv *testSSHServer)) {
	t.Helper()
	home = t.TempDir()
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pubKey, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "id_ed25519"), pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	pub, err = ssh.NewPublicKey(pubKey)
	if err != nil {
		t.Fatal(err)
	}
	finish = func(srv *testSSHServer) {
		_, port, _ := net.SplitHostPort(srv.addr)
		line := knownhosts.Line([]string{"[127.0.0.1]:" + port}, srv.hostPub)
		if err := os.WriteFile(filepath.Join(dir, "known_hosts"), []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := fmt.Sprintf("Host 127.0.0.1\n  IdentityFile %s\n  IdentitiesOnly yes\n  UserKnownHostsFile %s\n  StrictHostKeyChecking yes\n",
			filepath.Join(dir, "id_ed25519"), filepath.Join(dir, "known_hosts"))
		cfgPath := filepath.Join(dir, "config")
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		orig := sshBaseArgs
		sshBaseArgs = append([]string{"-F", cfgPath}, orig...)
		t.Cleanup(func() { sshBaseArgs = orig })
	}
	return home, pub, finish
}

// TestEndToEndOverRealSSH: discovery script over the real ssh binary, the -L
// forward, and a bearer-token Hello through it; then a remote-daemon restart
// (new port + token) is healed by the next Resolve; then Close kills ssh.
func TestEndToEndOverRealSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh binary on PATH")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}

	// The fake remote: a ~/.fleet whose ssh.port names an in-process daemon.
	remoteHome := t.TempDir()
	remoteFleet := filepath.Join(remoteHome, ".fleet")
	if err := os.MkdirAll(remoteFleet, 0o700); err != nil {
		t.Fatal(err)
	}
	rd := startRemoteDaemon(t, "tok-1")
	writeRemoteFiles := func(port int, token string) {
		for name, val := range map[string]string{"ssh.port": strconv.Itoa(port), "mcp.token": token + "\n", "server.version": "test"} {
			if err := os.WriteFile(filepath.Join(remoteFleet, name), []byte(val), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeRemoteFiles(rd.port, "tok-1")

	_, clientPub, finish := clientSSHHome(t)
	srv := startTestSSHServer(t, clientPub, remoteHome)
	finish(srv)
	t.Setenv("SSH_AUTH_SOCK", "")

	_, port, _ := net.SplitHostPort(srv.addr)
	rawURL := "ssh://tester@127.0.0.1:" + port

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(ctx)
	defer m.Close()

	ep, err := m.Resolve(ctx, rawURL)
	if err != nil {
		t.Fatalf("Resolve over real ssh: %v", err)
	}
	if ep.Token != "tok-1" {
		t.Fatalf("discovered token = %q", ep.Token)
	}
	// The endpoint is usable by an ordinary client with the token.
	if err := helloThrough(ctx, ep.Addr, ep.Token); err != nil {
		t.Fatalf("Hello through the forward: %v", err)
	}
	if err := helloThrough(ctx, ep.Addr, "wrong"); err == nil {
		t.Fatal("the remote must still enforce its token through the tunnel")
	}

	// Remote daemon restarts elsewhere with a new token: the next Resolve
	// notices the stale forward, re-discovers, and rebuilds on the same local
	// address.
	rd.srv.Stop()
	rd2 := startRemoteDaemon(t, "tok-2")
	writeRemoteFiles(rd2.port, "tok-2")
	ep2, err := m.Resolve(ctx, rawURL)
	if err != nil {
		t.Fatalf("Resolve after remote restart: %v", err)
	}
	if ep2.Addr != ep.Addr || ep2.Token != "tok-2" {
		t.Fatalf("after restart: %+v (was %+v)", ep2, ep)
	}
	if err := helloThrough(ctx, ep2.Addr, ep2.Token); err != nil {
		t.Fatalf("Hello after rebuild: %v", err)
	}

	// SSH mode switched off on the remote: discovery reports it by name.
	_ = os.Remove(filepath.Join(remoteFleet, "ssh.port"))
	rd2.srv.Stop()
	_, err = m.Resolve(ctx, rawURL)
	if !errors.Is(err, ErrSSHModeOff) {
		t.Fatalf("want ErrSSHModeOff, got %v", err)
	}

	m.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", ep.Addr, 100*time.Millisecond); err != nil {
			return
		} else {
			_ = c.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("Close should have killed the ssh forward")
}

// TestEndToEndAuthFailureIsReported: a client key the server doesn't know
// fails discovery with ssh's own diagnostic (batch mode: no prompt, no hang).
func TestEndToEndAuthFailureIsReported(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh binary on PATH")
	}
	_, _, finish := clientSSHHome(t)
	_, otherPub, _ := clientSSHHome(t) // a different key: the server won't accept ours
	srv := startTestSSHServer(t, otherPub, t.TempDir())
	finish(srv)
	t.Setenv("SSH_AUTH_SOCK", "")

	_, port, _ := net.SplitHostPort(srv.addr)
	m := New(context.Background())
	defer m.Close()
	start := time.Now()
	_, err := m.Resolve(context.Background(), "ssh://tester@127.0.0.1:"+port)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("Permission denied")) {
		t.Fatalf("want ssh's Permission denied, got %v", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("auth failure took %s — batch mode should fail fast", time.Since(start))
	}
}

// ===========================================
// Host-key prompt flow over the real ssh binary
// ===========================================

// clientSSHHomeWith is clientSSHHome with control over the known_hosts the
// client starts from: trust=false leaves it empty (the host is unknown);
// wrongKey pre-trusts a DIFFERENT key under the server's name (the host is
// known but changed).
func clientSSHHomeWith(t *testing.T, trust bool, wrongKey bool) (knownHosts string, pub ssh.PublicKey, finish func(srv *testSSHServer)) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pubKey, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "id_ed25519"), pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	pub, err = ssh.NewPublicKey(pubKey)
	if err != nil {
		t.Fatal(err)
	}
	knownHosts = filepath.Join(dir, "known_hosts")
	finish = func(srv *testSSHServer) {
		_, port, _ := net.SplitHostPort(srv.addr)
		name := "[127.0.0.1]:" + port
		switch {
		case wrongKey:
			otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
			other, _ := ssh.NewPublicKey(otherPub)
			if err := os.WriteFile(knownHosts, []byte(knownhosts.Line([]string{name}, other)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		case trust:
			if err := os.WriteFile(knownHosts, []byte(knownhosts.Line([]string{name}, srv.hostPub)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cfg := fmt.Sprintf("Host 127.0.0.1\n  IdentityFile %s\n  IdentitiesOnly yes\n  UserKnownHostsFile %s\n",
			filepath.Join(dir, "id_ed25519"), knownHosts)
		cfgPath := filepath.Join(dir, "config")
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		orig := sshBaseArgs
		sshBaseArgs = append([]string{"-F", cfgPath}, orig...)
		t.Cleanup(func() { sshBaseArgs = orig })
	}
	return knownHosts, pub, finish
}

func requireSSHTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ssh", "ssh-keyscan", "ssh-keygen", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on PATH", tool)
		}
	}
}

// TestEndToEndUnknownHostKeyAccept: an unknown host is refused with a
// structured error carrying the fingerprint ssh-keyscan/ssh-keygen computed;
// trusting the offered line appends exactly it to known_hosts and connects.
func TestEndToEndUnknownHostKeyAccept(t *testing.T) {
	requireSSHTools(t)
	remoteHome := t.TempDir()
	remoteFleet := filepath.Join(remoteHome, ".fleet")
	if err := os.MkdirAll(remoteFleet, 0o700); err != nil {
		t.Fatal(err)
	}
	rd := startRemoteDaemon(t, "tok-1")
	for name, val := range map[string]string{"ssh.port": strconv.Itoa(rd.port), "mcp.token": "tok-1\n", "server.version": "test"} {
		if err := os.WriteFile(filepath.Join(remoteFleet, name), []byte(val), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	knownHosts, clientPub, finish := clientSSHHomeWith(t, false, false)
	srv := startTestSSHServer(t, clientPub, remoteHome)
	finish(srv)
	t.Setenv("SSH_AUTH_SOCK", "")
	_, port, _ := net.SplitHostPort(srv.addr)
	rawURL := "ssh://tester@127.0.0.1:" + port

	m := New(context.Background())
	defer m.Close()

	_, err := m.Resolve(context.Background(), rawURL)
	var uk *UnknownHostKeyError
	if !errors.As(err, &uk) {
		t.Fatalf("want UnknownHostKeyError, got %T: %v", err, err)
	}
	if uk.Name != "[127.0.0.1]:"+port || uk.Host != "127.0.0.1" || strconv.Itoa(uk.Port) != port || uk.KeyType != "ED25519" {
		t.Fatalf("unknown key identity: %+v", uk)
	}
	if uk.KnownHostsPath != knownHosts {
		t.Fatalf("known_hosts path = %s, want the config's %s", uk.KnownHostsPath, knownHosts)
	}
	if len(uk.Keys) == 0 || uk.Keys[0].Type != "ssh-ed25519" || uk.Keys[0].Fingerprint != ssh.FingerprintSHA256(srv.hostPub) {
		t.Fatalf("offered keys: %+v (want the server's ed25519 key %s first)", uk.Keys, ssh.FingerprintSHA256(srv.hostPub))
	}
	wantLine := knownhosts.Line([]string{"[127.0.0.1]:" + port}, srv.hostPub)
	if uk.Keys[0].Line != wantLine {
		t.Fatalf("known_hosts line = %q, want %q", uk.Keys[0].Line, wantLine)
	}
	if !strings.Contains(err.Error(), "not known") || !strings.Contains(err.Error(), "SHA256:") {
		t.Fatalf("message should say the key is unknown and show the fingerprint: %v", err)
	}
	if _, err := os.Stat(knownHosts); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("nothing may be written to known_hosts before the user accepts")
	}

	ep, err := m.TrustHostKey(context.Background(), rawURL, uk.Keys[0].Line)
	if err != nil {
		t.Fatalf("TrustHostKey: %v", err)
	}
	if got, _ := os.ReadFile(knownHosts); string(got) != wantLine+"\n" {
		t.Fatalf("known_hosts after accept = %q, want exactly the offered line", got)
	}
	if err := helloThrough(context.Background(), ep.Addr, ep.Token); err != nil {
		t.Fatalf("Hello through the trusted tunnel: %v", err)
	}
	// The offer is consumed: accepting again is refused, nothing is rewritten.
	if _, err := m.TrustHostKey(context.Background(), rawURL, uk.Keys[0].Line); !errors.Is(err, ErrKeyNotOffered) {
		t.Fatalf("second accept: want ErrKeyNotOffered, got %v", err)
	}
}

// TestEndToEndUnknownHostKeyReject: without an accept nothing is ever
// written, and the next resolve refuses the host the same way.
func TestEndToEndUnknownHostKeyReject(t *testing.T) {
	requireSSHTools(t)
	knownHosts, clientPub, finish := clientSSHHomeWith(t, false, false)
	srv := startTestSSHServer(t, clientPub, t.TempDir())
	finish(srv)
	t.Setenv("SSH_AUTH_SOCK", "")
	_, port, _ := net.SplitHostPort(srv.addr)
	rawURL := "ssh://tester@127.0.0.1:" + port

	m := New(context.Background())
	defer m.Close()
	for i := 0; i < 2; i++ {
		_, err := m.Resolve(context.Background(), rawURL)
		var uk *UnknownHostKeyError
		if !errors.As(err, &uk) {
			t.Fatalf("resolve %d: want UnknownHostKeyError, got %v", i, err)
		}
	}
	if _, err := os.Stat(knownHosts); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a rejected (never accepted) key must not touch known_hosts")
	}
}

// TestEndToEndChangedHostKeyRefused: a known host presenting a different key
// is refused with the offending known_hosts file:line and is NEVER offered.
func TestEndToEndChangedHostKeyRefused(t *testing.T) {
	requireSSHTools(t)
	knownHosts, clientPub, finish := clientSSHHomeWith(t, false, true)
	srv := startTestSSHServer(t, clientPub, t.TempDir())
	finish(srv)
	t.Setenv("SSH_AUTH_SOCK", "")
	_, port, _ := net.SplitHostPort(srv.addr)
	rawURL := "ssh://tester@127.0.0.1:" + port
	before, _ := os.ReadFile(knownHosts)

	m := New(context.Background())
	defer m.Close()
	_, err := m.Resolve(context.Background(), rawURL)
	var changed *ChangedHostKeyError
	if !errors.As(err, &changed) {
		t.Fatalf("want ChangedHostKeyError, got %T: %v", err, err)
	}
	var uk *UnknownHostKeyError
	if errors.As(err, &uk) {
		t.Fatal("a changed key must never be offered for acceptance")
	}
	if changed.File != knownHosts || changed.Line != 1 || !strings.Contains(err.Error(), knownHosts+":1") {
		t.Fatalf("changed key should name %s:1, got %+v / %v", knownHosts, changed, err)
	}
	if len(m.offered) != 0 {
		t.Fatal("no offer may be recorded for a changed key")
	}
	if after, _ := os.ReadFile(knownHosts); string(after) != string(before) {
		t.Fatal("known_hosts must be untouched")
	}
}
