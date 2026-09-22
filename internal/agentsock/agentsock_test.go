package agentsock

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestModeFor(t *testing.T) {
	cases := []struct {
		override, goos string
		want           Mode
	}{
		{"", "linux", ModeRelay},
		{"", "darwin", ModeDockerDesktop},
		{"off", "linux", ModeOff},
		{"NONE", "darwin", ModeOff},
		{"/run/host-services/ssh-auth.sock", "linux", ModeOverride},
		{"/custom.sock", "darwin", ModeOverride},
	}
	for _, c := range cases {
		if got := ModeFor(c.override, c.goos); got != c.want {
			t.Errorf("ModeFor(%q, %q) = %v, want %v", c.override, c.goos, got, c.want)
		}
	}
}

func TestOriginSock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Setenv(EnvAuthSock, "/tmp/agent.sock")
	if got := OriginSock(); got != "/tmp/agent.sock" {
		t.Fatalf("before redirect: got %q", got)
	}

	// The daemon redirected SSH_AUTH_SOCK to its relay and kept the origin.
	t.Setenv(EnvAuthSock, HostSocketPath())
	t.Setenv(EnvOrigin, "/tmp/agent.sock")
	if got := OriginSock(); got != "/tmp/agent.sock" {
		t.Fatalf("after redirect: got %q", got)
	}

	// Started without an agent: the empty origin must win over the relay.
	t.Setenv(EnvOrigin, "")
	if got := OriginSock(); got != "" {
		t.Fatalf("empty origin: got %q", got)
	}
}

func TestOriginSockNeverTheRelay(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// A daemon re-spawned by one of its own children inherits SSH_AUTH_SOCK
	// pointing at the old daemon's relay and no origin variable.
	t.Setenv(EnvAuthSock, HostSocketPath())
	if got := OriginSock(); got != "" {
		t.Fatalf("got %q, want the relay filtered out", got)
	}
}

// liveSocket binds a unix socket under a short temp dir.
func liveSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ags")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return sock
}

func TestWithOriginAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	origin := liveSocket(t)
	relay := HostSocketPath()
	t.Setenv(EnvAuthSock, relay)
	t.Setenv(EnvOrigin, origin)

	got := WithOriginAgent([]string{"HOME=/h", EnvAuthSock + "=" + relay})
	if !slices.Equal(got, []string{"HOME=/h", EnvAuthSock + "=" + origin}) {
		t.Fatalf("relay not swapped for the live origin: %v", got)
	}
	// Someone else's SSH_AUTH_SOCK is left alone.
	other := []string{EnvAuthSock + "=/elsewhere.sock"}
	if got := WithOriginAgent(other); !slices.Equal(got, other) {
		t.Fatalf("a non-relay SSH_AUTH_SOCK was changed: %v", got)
	}
	// No live origin (a remote daemon started without one): keep the relay —
	// a mount that works until the next restart beats an empty source.
	t.Setenv(EnvOrigin, "/tmp/gone.sock")
	in := []string{EnvAuthSock + "=" + relay}
	if got := WithOriginAgent(in); !slices.Equal(got, in) {
		t.Fatalf("relay swapped for a dead origin: %v", got)
	}
}

func TestRelayUsable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() {
		SetRelayServing(false)
		SetRemoteClients(false)
		SetProviderSeen(false)
	})
	t.Setenv(EnvOrigin, "")
	t.Setenv(EnvAuthSock, "")

	SetRelayServing(false)
	SetRemoteClients(true)
	SetProviderSeen(true)
	if RelayUsable() {
		t.Fatal("not serving and nothing published: not usable")
	}
	SetRelayServing(true)
	if !RelayUsable() {
		t.Fatal("serving, remote clients possible, a provider seen before: usable")
	}
	SetProviderSeen(false)
	if RelayUsable() {
		t.Fatal("Remote Fleet on but nobody ever forwarded an agent here: nothing could answer")
	}
	SetProviderSeen(true)
	SetRemoteClients(false)
	if RelayUsable() {
		t.Fatal("no agent of its own and no remote clients: nothing could answer")
	}
	t.Setenv(EnvAuthSock, liveSocket(t))
	if !RelayUsable() {
		t.Fatal("the daemon's own agent backs it")
	}
}

func TestRelayUsablePublishedForOtherProcesses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvOrigin, "")
	t.Setenv(EnvAuthSock, liveSocket(t))
	if err := os.MkdirAll(filepath.Dir(HostSocketPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetRelayServing(false) })

	// The daemon serves and has an agent: it publishes "usable" for a CLI
	// process that runs a backend in-process (`fleet start`).
	SetRelayServing(true)
	if _, err := os.Stat(usablePath()); err != nil {
		t.Fatalf("verdict not published: %v", err)
	}
	relayServing.Store(false) // what a CLI process sees
	if !RelayUsable() {
		t.Fatal("a non-daemon process must read the published verdict")
	}
	relayServing.Store(true)
	// Shutting the relay down withdraws it.
	SetRelayServing(false)
	if RelayUsable() {
		t.Fatal("the verdict must be withdrawn when the relay stops")
	}
}
