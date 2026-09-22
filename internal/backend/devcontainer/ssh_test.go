package devcontainer

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
)

// shortSocketPath returns a socket path short enough to bind. t.TempDir()
// on macOS lives under /var/folders/... and, with the test name appended,
// exceeds the platform's 104-byte sun_path limit.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fmssh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "agent.sock")
}

func TestHostSSHAuthSock_Unset(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	if got := hostSSHAuthSock(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestHostSSHAuthSock_NonexistentPath(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/no-such-socket-"+t.Name())
	if got := hostSSHAuthSock(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestHostSSHAuthSock_RegularFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "not-a-socket")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Setenv("SSH_AUTH_SOCK", f.Name())
	if got := hostSSHAuthSock(); got != "" {
		t.Errorf("expected empty for regular file, got %q", got)
	}
}

func TestHostSSHAuthSock_ValidSocket(t *testing.T) {
	sockPath := shortSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	t.Setenv("SSH_AUTH_SOCK", sockPath)
	if got := hostSSHAuthSock(); got != sockPath {
		t.Errorf("expected %q, got %q", sockPath, got)
	}
}

func TestAgentPlanFor(t *testing.T) {
	relay := agentPlan{sock: agentsock.ContainerSocketPath}
	direct := func(mount string) agentPlan { return agentPlan{mount: mount, sock: containerSSHSocketPath} }
	cases := []struct {
		name, override, hostSock, goos string
		want                           agentPlan
	}{
		{"linux uses the relay", "", "/tmp/agent.sock", "linux", relay},
		{"linux relay needs no agent up front", "", "", "linux", relay},
		{"darwin mounts the docker vm path", "", "/private/tmp/launchd/Listeners", "darwin", direct(dockerDesktopSSHAuthSock)},
		{"no agent on darwin, no mount", "", "", "darwin", agentPlan{}},
		{"override wins without host agent", "/vm/custom.sock", "", "darwin", direct("/vm/custom.sock")},
		{"override wins over the relay", "/vm/custom.sock", "/tmp/agent.sock", "linux", direct("/vm/custom.sock")},
		{"off disables", "off", "/tmp/agent.sock", "linux", agentPlan{}},
		{"none disables", "none", "/tmp/agent.sock", "darwin", agentPlan{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode := agentsock.ModeFor(tc.override, tc.goos)
			if got := agentPlanFor(mode, tc.override, tc.hostSock); got != tc.want {
				t.Errorf("agentPlanFor(%q, %q, %q) = %+v, want %+v", tc.override, tc.hostSock, tc.goos, got, tc.want)
			}
		})
	}
}

// wantAgentSock is the SSH_AUTH_SOCK instances get on this platform with no
// override and a live agent.
func wantAgentSock() string {
	if runtime.GOOS == "darwin" {
		return containerSSHSocketPath
	}
	return agentsock.ContainerSocketPath
}

func TestSSHUpArgs_WithValidSocket(t *testing.T) {
	liveAgentSocket(t)
	args, err := sshUpArgs()
	if err != nil {
		t.Fatalf("sshUpArgs: %v", err)
	}
	want := []string{"--remote-env", "SSH_AUTH_SOCK=" + wantAgentSock()}
	if runtime.GOOS == "darwin" {
		want = append([]string{"--mount", "type=bind,source=" + dockerDesktopSSHAuthSock + ",target=" + containerSSHSocketPath}, want...)
	}
	if !slices.Equal(args, want) {
		t.Fatalf("sshUpArgs() = %v, want %v", args, want)
	}
}

func TestSSHUpArgs_OverridePathMountsIt(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv(sshAgentSockOverrideEnv, "/run/host-services/ssh-auth.sock")
	args, err := sshUpArgs()
	if err != nil {
		t.Fatalf("sshUpArgs: %v", err)
	}
	want := []string{
		"--mount", "type=bind,source=/run/host-services/ssh-auth.sock,target=" + containerSSHSocketPath,
		"--remote-env", "SSH_AUTH_SOCK=" + containerSSHSocketPath,
	}
	if !slices.Equal(args, want) {
		t.Fatalf("sshUpArgs() = %v, want %v", args, want)
	}
}

func TestSSHUpArgs_OverrideOff(t *testing.T) {
	liveAgentSocket(t)
	t.Setenv(sshAgentSockOverrideEnv, "off")
	if args, err := sshUpArgs(); err != nil || args != nil {
		t.Errorf("expected nil/nil with %s=off, got %v, %v", sshAgentSockOverrideEnv, args, err)
	}
}

func TestSSHUpArgs_OverrideCaseAndWhitespace(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	// OFF and padded off both disable forwarding rather than becoming a
	// literal bind source.
	for _, v := range []string{"OFF", " None ", "off "} {
		t.Setenv(sshAgentSockOverrideEnv, v)
		if args, err := sshUpArgs(); err != nil || args != nil {
			t.Errorf("%s=%q: expected nil/nil, got %v, %v", sshAgentSockOverrideEnv, v, args, err)
		}
	}
}

func TestSSHUpArgs_OverrideInvalid(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	// Relative paths and comma-splicing into the mount string are hard
	// errors, not confusing downstream docker failures.
	for _, v := range []string{"relative/path.sock", "/run/foo.sock,readonly", "true"} {
		t.Setenv(sshAgentSockOverrideEnv, v)
		if _, err := sshUpArgs(); err == nil {
			t.Errorf("%s=%q: expected error, got none", sshAgentSockOverrideEnv, v)
		}
	}
}

func TestSSHUpArgs_NoSocket(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv(sshAgentSockOverrideEnv, "")
	args, err := sshUpArgs()
	if err != nil {
		t.Fatalf("sshUpArgs: %v", err)
	}
	if runtime.GOOS == "darwin" {
		if args != nil {
			t.Errorf("darwin without an agent: expected nil, got %v", args)
		}
		return
	}
	// The relay serves whatever agent is reachable per connection, so an
	// instance is pointed at it even when none is up yet.
	if want := []string{"--remote-env", "SSH_AUTH_SOCK=" + agentsock.ContainerSocketPath}; !slices.Equal(args, want) {
		t.Errorf("sshUpArgs() = %v, want %v", args, want)
	}
}

func TestSSHAgentMountSource_RelayMountsNothing(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("the relay is not used for instances on macOS")
	}
	liveAgentSocket(t)
	if got, err := sshAgentMountSource(); err != nil || got != "" {
		t.Fatalf("sshAgentMountSource() = %q, %v; want no socket-file mount", got, err)
	}
}

// liveAgentSocket binds a real unix socket and points SSH_AUTH_SOCK at it,
// with the override cleared (the macOS Docker Desktop gate stats the socket,
// so a bare env var is not enough there).
func liveAgentSocket(t *testing.T) {
	t.Helper()
	sockPath := shortSocketPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	t.Setenv("SSH_AUTH_SOCK", sockPath)
	t.Setenv(sshAgentSockOverrideEnv, "")
}

func TestSSHExecArgs_WithAgent(t *testing.T) {
	liveAgentSocket(t)
	args := sshExecArgs()
	if want := []string{"--remote-env", "SSH_AUTH_SOCK=" + wantAgentSock()}; !slices.Equal(args, want) {
		t.Fatalf("sshExecArgs() = %v, want %v", args, want)
	}
}

func TestSSHExecArgs_OverrideOff(t *testing.T) {
	liveAgentSocket(t)
	t.Setenv(sshAgentSockOverrideEnv, "off")
	if args := sshExecArgs(); args != nil {
		t.Errorf("expected nil with %s=off, got %v", sshAgentSockOverrideEnv, args)
	}
}

func TestExecArgs_WithSSH(t *testing.T) {
	liveAgentSocket(t)
	args := execArgs("/workspace", []string{"bash"})
	expected := []string{
		"exec", "--workspace-folder", "/workspace",
		"--remote-env", "SSH_AUTH_SOCK=" + wantAgentSock(),
		"bash",
	}
	if !slices.Equal(args, expected) {
		t.Fatalf("got %v, want %v", args, expected)
	}
}

func TestExecArgs_WithoutSSH(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv(sshAgentSockOverrideEnv, "off")
	args := execArgs("/workspace", []string{"bash"})
	expected := []string{"exec", "--workspace-folder", "/workspace", "bash"}
	if !slices.Equal(args, expected) {
		t.Fatalf("got %v, want %v", args, expected)
	}
}
