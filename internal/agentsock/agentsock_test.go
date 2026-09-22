package agentsock

import "testing"

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
