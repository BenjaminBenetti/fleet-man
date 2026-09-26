package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/server/sshtunnel"
	"github.com/BenjaminBenetti/fleet-man/internal/shellquote"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Real git, OpenSSH, ssh-keyscan and ssh-keygen talk to an in-process git SSH
// server. A PATH wrapper supplies -F because OpenSSH ignores HOME for config.
// All keys/config/state are temporary; no developer known_hosts is touched.
type gitSSHFixture struct {
	remote, knownHosts, config, name, line, fingerprint string
	uploads                                             atomic.Int32
}

func newGitSSHFixture(t *testing.T) *gitSSHFixture {
	t.Helper()
	for _, name := range []string{"git", "ssh", "ssh-keyscan", "ssh-keygen"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s unavailable", name)
		}
	}
	isolateFleetDir(t)
	for _, name := range []string{"GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSH_VARIANT"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	repo := filepath.Join(t.TempDir(), "repo")
	runGit := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "topic", repo)
	if err := os.MkdirAll(filepath.Join(repo, ".devcontainer"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".devcontainer", "devcontainer.json"), []byte(`{"image":"unused:test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("-C", repo, "add", ".")
	runGit("-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	f := &gitSSHFixture{
		remote:      "ssh://git@127.0.0.1:" + port + "/repo.git",
		knownHosts:  filepath.Join(t.TempDir(), ".ssh", "known_hosts"),
		config:      filepath.Join(t.TempDir(), "config"),
		name:        "[127.0.0.1]:" + port,
		fingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
	}
	f.line = f.name + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				stop := context.AfterFunc(ctx, func() { c.Close() })
				defer stop()
				conn, channels, requests, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				defer conn.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						incoming.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					ch, reqs, err := incoming.Accept()
					if err != nil {
						return
					}
					for req := range reqs {
						if req.Type != "exec" {
							req.Reply(false, nil)
							continue
						}
						f.uploads.Add(1)
						req.Reply(true, nil)
						cmd := exec.CommandContext(ctx, "git", "upload-pack", repo)
						cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
						exit := uint32(0)
						if cmd.Run() != nil {
							exit = 1
						}
						var data [4]byte
						binary.BigEndian.PutUint32(data[:], exit)
						ch.SendRequest("exit-status", false, data[:])
						break
					}
					ch.Close()
				}
			}()
		}
	}()
	t.Cleanup(func() { cancel(); ln.Close(); wg.Wait() })
	config := fmt.Sprintf("Host *\n  UserKnownHostsFile %s\n  GlobalKnownHostsFile /dev/null\n  IdentityAgent none\n  IdentityFile none\n  StrictHostKeyChecking ask\n", f.knownHosts)
	if err := os.WriteFile(f.config, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	sshPath, _ := exec.LookPath("ssh")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexec "+shellquote.Single(sshPath)+" -F "+shellquote.Single(f.config)+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A create job can verify its clone without starting any real containers.
	if err := os.WriteFile(filepath.Join(bin, "devcontainer"), []byte("#!/bin/sh\necho fixture-provisioning-reached >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

func TestGitHostKeyInspectOverRemoteConnection(t *testing.T) {
	for _, action := range []string{"accept", "reject", "disconnect", "unattended", "changed", "forged", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f := newGitSSHFixture(t)
			svc := newService()
			unary, streaming := bearerAuthInterceptors("secret")
			gs := grpc.NewServer(grpc.ChainUnaryInterceptor(unary), grpc.ChainStreamInterceptor(streaming))
			fleetgrpc.RegisterFleetServiceServer(gs, svc)
			client := dialBuf(t, gs)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer secret")
			var before []byte
			if action == "changed" {
				_, p, _ := ed25519.GenerateKey(rand.Reader)
				other, _ := ssh.NewSignerFromKey(p)
				before = []byte(f.name + " " + string(ssh.MarshalAuthorizedKey(other.PublicKey())))
				os.MkdirAll(filepath.Dir(f.knownHosts), 0o700)
				if err := os.WriteFile(f.knownHosts, before, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var prompts atomic.Int32
			providerCtx, disconnect := context.WithCancel(ctx)
			defer disconnect()
			if action != "unattended" {
				stop, err := fleetclient.StartGitHostKeyPrompts(providerCtx, client, func(pctx context.Context, key *fleetgrpc.UnknownSSHHostKey) string {
					prompts.Add(1)
					if key.GetName() != f.name || key.GetKnownHostsPath() != f.knownHosts || key.GetUrl() != f.remote || key.GetKeys()[0].GetFingerprint() != f.fingerprint || key.GetKeys()[0].GetKnownHostsLine() != f.line {
						t.Errorf("wrong host-key offer: %v", key)
					}
					if _, err := os.Stat(f.knownHosts); !os.IsNotExist(err) {
						t.Errorf("key written before acceptance: %v", err)
					}
					switch action {
					case "accept":
						return key.GetKeys()[0].GetKnownHostsLine()
					case "forged":
						return "other-host ssh-ed25519 AAAA"
					case "disconnect":
						disconnect()
					case "cancel":
						cancel()
						<-pctx.Done()
					}
					return ""
				})
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
			}
			reply, err := client.InspectRepo(ctx, &fleetgrpc.InspectRepoRequest{RemoteUrl: f.remote, Branch: "topic"})
			if action == "accept" {
				if err != nil || !reply.GetHasDevcontainer() {
					t.Fatalf("inspect after accept: reply=%v err=%v", reply, err)
				}
				data, err := os.ReadFile(f.knownHosts)
				if err != nil || string(data) != f.line+"\n" || f.uploads.Load() != 1 {
					t.Fatalf("saved=%q err=%v uploads=%d", data, err, f.uploads.Load())
				}
				// A second clone must use the saved key without asking again.
				if _, err := client.InspectRepo(ctx, &fleetgrpc.InspectRepoRequest{RemoteUrl: f.remote}); err != nil {
					t.Fatal(err)
				}
				if prompts.Load() != 1 {
					t.Fatalf("prompted %d times", prompts.Load())
				}
			} else {
				if err == nil {
					t.Fatal("clone should fail without trust")
				}
				if action != "cancel" && !strings.Contains(err.Error(), "hint:") {
					t.Fatalf("missing actionable hint: %v", err)
				}
				data, _ := os.ReadFile(f.knownHosts)
				if string(data) != string(before) || f.uploads.Load() != 0 {
					t.Fatalf("unexpected trust/clone: saved=%q uploads=%d", data, f.uploads.Load())
				}
				wantPrompts := int32(1)
				if action == "unattended" || action == "changed" {
					wantPrompts = 0
				}
				if prompts.Load() != wantPrompts {
					t.Fatalf("prompts=%d want=%d", prompts.Load(), wantPrompts)
				}
			}
		})
	}
}

func TestGitHostKeyDetachedCreateJob(t *testing.T) {
	f := newGitSSHFixture(t)
	_, client, cleanup := newTestServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	asked := make(chan struct{})
	accept := make(chan struct{})
	stop, err := fleetclient.StartGitHostKeyPrompts(ctx, client, func(ctx context.Context, key *fleetgrpc.UnknownSSHHostKey) string {
		close(asked)
		select {
		case <-accept:
			return key.GetKeys()[0].GetKnownHostsLine()
		case <-ctx.Done():
			return ""
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	jobCtx, detach := context.WithCancel(ctx)
	defer detach()
	stream, err := client.CreateInstance(jobCtx, &fleetgrpc.CreateInstanceRequest{Fleet: "hostkey", Instance: "one", Remote: &f.remote})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetStarted() == nil {
		t.Fatalf("start: %v %v", first, err)
	}
	detach() // the TUI detaches its job stream at precisely this point
	select {
	case <-asked:
	case <-ctx.Done():
		t.Fatal("detached job never prompted")
	}
	close(accept)
	// Wait for the persisted final state, not just the callback, so no job
	// goroutine can outlive the test's temporary HOME.
	for ctx.Err() == nil {
		reply, err := client.GetState(ctx, &fleetgrpc.GetStateRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if len(reply.GetActiveJobs()) == 0 {
			if f.uploads.Load() != 1 {
				t.Fatalf("create did not clone after acceptance: %v", reply)
			}
			if !strings.Contains(reply.String(), "fixture-provisioning-reached") {
				t.Fatalf("job did not reach the stub backend: %v", reply)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("create job never finished")
}

func TestGitHostKeyStreamRequiresRemoteToken(t *testing.T) {
	_, streaming := bearerAuthInterceptors("secret")
	gs := grpc.NewServer(grpc.ChainStreamInterceptor(streaming))
	fleetgrpc.RegisterFleetServiceServer(gs, newService())
	client := dialBuf(t, gs)
	stop, err := fleetclient.StartGitHostKeyPrompts(context.Background(), client, func(context.Context, *fleetgrpc.UnknownSSHHostKey) string {
		t.Error("unauthenticated client received a prompt")
		return ""
	})
	defer stop()
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}

func TestGitHostKeyCancelledRequestWithdrawsPrompt(t *testing.T) {
	svc, client, cleanup := newTestServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	asked, withdrawn := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	stop, err := fleetclient.StartGitHostKeyPrompts(ctx, client, func(ctx context.Context, key *fleetgrpc.UnknownSSHHostKey) string {
		if calls.Add(1) == 1 {
			close(asked)
			<-ctx.Done()
			close(withdrawn)
			return ""
		}
		return key.GetKeys()[0].GetKnownHostsLine()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	key := &sshtunnel.UnknownHostKeyError{Name: "server", Keys: []sshtunnel.HostKey{{Line: "server ssh-ed25519 AAAA"}}}
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	result := make(chan error, 1)
	go func() { _, err := svc.gitHostKeys.ask(requestCtx, "git@server:repo", key); result <- err }()
	select {
	case <-asked:
	case <-ctx.Done():
		t.Fatal("no prompt")
	}
	cancelRequest()
	select {
	case <-withdrawn:
	case <-ctx.Done():
		t.Fatal("prompt was not withdrawn when its inspection ended")
	}
	if err := <-result; err == nil {
		t.Fatal("cancelled request succeeded")
	}
	// Cancelling one inspection must leave the connected client usable.
	if line, err := svc.gitHostKeys.ask(ctx, "git@server:next", key); err != nil || line != key.Keys[0].Line {
		t.Fatalf("next approval: %q %v", line, err)
	}
}

func TestGitHostKeyConcurrentInspectionsWriteOnce(t *testing.T) {
	f := newGitSSHFixture(t)
	_, client, cleanup := newTestServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	asked := make(chan struct{}, 2)
	accept := make(chan struct{})
	stop, err := fleetclient.StartGitHostKeyPrompts(ctx, client, func(ctx context.Context, key *fleetgrpc.UnknownSSHHostKey) string {
		asked <- struct{}{}
		select {
		case <-accept:
			return key.GetKeys()[0].GetKnownHostsLine()
		case <-ctx.Done():
			return ""
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := client.InspectRepo(ctx, &fleetgrpc.InspectRepoRequest{RemoteUrl: f.remote})
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-asked:
		case <-ctx.Done():
			t.Fatal("concurrent clones did not both prompt")
		}
	}
	close(accept)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(f.knownHosts)
	if err != nil || string(data) != f.line+"\n" {
		t.Fatalf("concurrent trust must append exactly once: %q %v", data, err)
	}
}

func TestGitHostKeyResolvesGitRewriteAndSSHConfig(t *testing.T) {
	f := newGitSSHFixture(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(strings.Split(f.remote, "/repo")[0], "ssh://git@"))
	cfg, _ := os.ReadFile(f.config)
	cfg = append(cfg, []byte("Host fixture-git\n  HostName 127.0.0.1\n  Port "+port+"\n  HostKeyAlias fixture-host-key\n")...)
	if err := os.WriteFile(f.config, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "config", "--global", "url.ssh://git@fixture-git/.insteadOf", "shortcut:").CombinedOutput(); err != nil {
		t.Fatalf("configure rewrite: %v %s", err, out)
	}
	_, client, cleanup := newTestServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stop, err := fleetclient.StartGitHostKeyPrompts(ctx, client, func(_ context.Context, key *fleetgrpc.UnknownSSHHostKey) string {
		if key.GetName() != "fixture-host-key" || key.GetHost() != "127.0.0.1" || key.GetUrl() != "shortcut:repo.git" || key.GetKeys()[0].GetFingerprint() != f.fingerprint {
			t.Errorf("wrong rewritten target: %v", key)
		}
		return key.GetKeys()[0].GetKnownHostsLine()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if _, err := client.InspectRepo(ctx, &fleetgrpc.InspectRepoRequest{RemoteUrl: "shortcut:repo.git"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.knownHosts)
	want := strings.Replace(f.line, f.name, "fixture-host-key", 1) + "\n"
	if err != nil || string(data) != want {
		t.Fatalf("saved=%q want=%q err=%v", data, want, err)
	}
}
