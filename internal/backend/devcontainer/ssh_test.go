package devcontainer

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
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
			if got := agentPlanFor(mode, tc.override, tc.hostSock, true); got != tc.want {
				t.Errorf("agentPlanFor(%q, %q, %q) = %+v, want %+v", tc.override, tc.hostSock, tc.goos, got, tc.want)
			}
		})
	}
	// Nothing can ever answer on the relay: no SSH_AUTH_SOCK at all, so an
	// in-container `ssh-agent` fallback still starts.
	if got := agentPlanFor(agentsock.ModeRelay, "", "", false); got != (agentPlan{}) {
		t.Errorf("unusable relay: got %+v, want no agent", got)
	}
}

// relayServing marks this process as serving the relay, as the daemon does.
// The agentsock setters publish (and on cleanup remove) a marker file under
// $HOME/.fleet, so HOME is pointed at a temp dir FIRST: t.Cleanup runs LIFO,
// so the setters' cleanups also run under it and never touch the developer's
// real ~/.fleet. Real docker lookups are forbidden too; a test that exercises
// one stubs it after this.
func relayServing(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	stubControlMountLookup(t, func(ws string) (bool, bool, error) {
		t.Errorf("unexpected docker lookup for %s", ws)
		return false, false, errors.New("no docker in unit tests")
	})
	agentsock.SetRelayServing(true)
	t.Cleanup(func() { agentsock.SetRelayServing(false) })
}

// stubControlMountLookup replaces the docker lookup behind hasControlMount
// with fn and starts from an empty cache, restoring both on cleanup.
func stubControlMountLookup(t *testing.T, fn func(workspaceDir string) (found, mounted bool, err error)) {
	t.Helper()
	orig := containerHasControlMount
	containerHasControlMount = fn
	resetControlMountCache()
	t.Cleanup(func() {
		containerHasControlMount = orig
		resetControlMountCache()
	})
}

func resetControlMountCache() {
	controlMountCache.Lock()
	clear(controlMountCache.answers)
	controlMountCache.Unlock()
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
	relayServing(t)
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
	relayServing(t)
	args, err := sshUpArgs()
	if err != nil {
		t.Fatalf("sshUpArgs: %v", err)
	}
	if args != nil {
		t.Errorf("no agent and no remote clients: expected nil, got %v", args)
	}
	if runtime.GOOS == "darwin" {
		return
	}
	// A remote client has provided its agent here and can again: the relay is
	// worth pointing at even though nothing backs it right now.
	agentsock.SetRemoteClients(true)
	agentsock.SetProviderSeen(true)
	t.Cleanup(func() {
		agentsock.SetRemoteClients(false)
		agentsock.SetProviderSeen(false)
	})
	args, err = sshUpArgs()
	if err != nil {
		t.Fatalf("sshUpArgs: %v", err)
	}
	if want := []string{"--remote-env", "SSH_AUTH_SOCK=" + agentsock.ContainerSocketPath}; !slices.Equal(args, want) {
		t.Errorf("sshUpArgs() = %v, want %v", args, want)
	}
}

func TestSSHAgentMountSource_RelayMountsNothing(t *testing.T) {
	relayServing(t)
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
	relayServing(t)
	liveAgentSocket(t)
	args := sshExecArgs("")
	if want := []string{"--remote-env", "SSH_AUTH_SOCK=" + wantAgentSock()}; !slices.Equal(args, want) {
		t.Fatalf("sshExecArgs = %v, want %v", args, want)
	}
}

func TestSSHExecArgs_OverrideOff(t *testing.T) {
	liveAgentSocket(t)
	t.Setenv(sshAgentSockOverrideEnv, "off")
	if args := sshExecArgs(""); args != nil {
		t.Errorf("expected nil with %s=off, got %v", sshAgentSockOverrideEnv, args)
	}
}

// instanceWorkspace makes <tmp>/<instance>/{.control,<workspace>} and
// returns the workspace path, laid out like the real thing: provisioned with
// the control directory mounted, so carrying provisioning's marker.
func instanceWorkspace(t *testing.T) string {
	t.Helper()
	ws := unmarkedInstanceWorkspace(t)
	if err := os.WriteFile(control.MountMarkerPath(filepath.Join(filepath.Dir(ws), control.HostDirName)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// unmarkedInstanceWorkspace is instanceWorkspace without the marker: the
// control directory as the daemon's control-socket registry creates it for
// any running instance, whatever its container mounts.
func unmarkedInstanceWorkspace(t *testing.T) string {
	t.Helper()
	inst := t.TempDir()
	if err := os.Mkdir(filepath.Join(inst, ".control"), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(inst, "workspace")
}

func TestExecArgs_WithSSH(t *testing.T) {
	relayServing(t)
	liveAgentSocket(t)
	ws := instanceWorkspace(t)
	args := execArgs(ws, []string{"bash"})
	expected := []string{
		"exec", "--workspace-folder", ws,
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

// lookupResult is a canned containerHasControlMount answer that counts calls.
type lookupResult struct {
	found, mounted bool
	err            error
	calls          int
}

func (r *lookupResult) lookup(string) (bool, bool, error) {
	r.calls++
	return r.found, r.mounted, r.err
}

var (
	relayExecArgs  = []string{"--remote-env", "SSH_AUTH_SOCK=" + agentsock.ContainerSocketPath}
	legacyExecArgs = []string{"--remote-env", "SSH_AUTH_SOCK=" + containerSSHSocketPath}
)

// relayExecTest sets up a Linux relay with a live agent, where sshExecArgs
// has to decide between the relay and the legacy socket-file mount.
func relayExecTest(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("the relay is not used for instances on macOS")
	}
	liveAgentSocket(t)
	relayServing(t)
}

func TestSSHExecArgs_MarkedInstanceUsesTheRelayWithoutDocker(t *testing.T) {
	relayExecTest(t)
	// relayServing forbids docker lookups: the marker alone decides.
	if got := sshExecArgs(instanceWorkspace(t)); !slices.Equal(got, relayExecArgs) {
		t.Fatalf("marked instance: %v, want %v", got, relayExecArgs)
	}
}

func TestSSHExecArgs_UnmarkedInstanceAsksDocker(t *testing.T) {
	cases := []struct {
		name   string
		result lookupResult
		want   []string
	}{
		// The daemon created .control for an instance whose container
		// predates the mount: only the agent socket file was bind-mounted,
		// at /run/ssh-agent.sock, so keep pointing it there.
		{"container without the mount keeps the old socket", lookupResult{found: true}, legacyExecArgs},
		{"container with the mount uses the relay", lookupResult{found: true, mounted: true}, relayExecArgs},
		{"no container yet assumes the relay", lookupResult{}, relayExecArgs},
		{"docker failing assumes the relay", lookupResult{err: errors.New("docker down")}, relayExecArgs},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			relayExecTest(t)
			stubControlMountLookup(t, c.result.lookup)
			if got := sshExecArgs(unmarkedInstanceWorkspace(t)); !slices.Equal(got, c.want) {
				t.Fatalf("sshExecArgs = %v, want %v", got, c.want)
			}
			if c.result.calls != 1 {
				t.Fatalf("docker lookups = %d, want 1", c.result.calls)
			}
		})
	}
}

func TestSSHExecArgs_NoControlDirAtAllKeepsTheOldMount(t *testing.T) {
	relayExecTest(t)
	stubControlMountLookup(t, (&lookupResult{found: true}).lookup)
	if got := sshExecArgs(filepath.Join(t.TempDir(), "fleet")); !slices.Equal(got, legacyExecArgs) {
		t.Fatalf("sshExecArgs = %v, want %v", got, legacyExecArgs)
	}
}

// fakeClock pins controlMountNow for the duration of the test.
func fakeClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Now()
	orig := controlMountNow
	controlMountNow = func() time.Time { return now }
	t.Cleanup(func() { controlMountNow = orig })
	return &now
}

func TestHasControlMount_CachesDockersAnswer(t *testing.T) {
	now := fakeClock(t)
	res := &lookupResult{found: true}
	stubControlMountLookup(t, res.lookup)
	ws := unmarkedInstanceWorkspace(t)

	for range 5 {
		if hasControlMount(ws) {
			t.Fatal("container without the mount: want the legacy socket")
		}
	}
	if res.calls != 1 {
		t.Fatalf("docker lookups = %d, want 1 (the answer is cached)", res.calls)
	}
	*now = now.Add(controlMountCacheTTL)
	hasControlMount(ws)
	if res.calls != 2 {
		t.Fatalf("docker lookups after the TTL = %d, want 2", res.calls)
	}

	// Rebuilt with the mount: provisioning's marker wins over the cached
	// answer at once.
	if err := os.WriteFile(control.MountMarkerPath(filepath.Join(filepath.Dir(ws), control.HostDirName)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasControlMount(ws) || res.calls != 2 {
		t.Fatalf("marked after a rebuild: want the relay without asking docker (lookups = %d)", res.calls)
	}
}

func TestHasControlMount_RetriesUncertainAnswersSoon(t *testing.T) {
	for _, res := range []*lookupResult{{err: errors.New("docker down")}, {}} {
		now := fakeClock(t)
		stubControlMountLookup(t, res.lookup)
		ws := unmarkedInstanceWorkspace(t)
		if !hasControlMount(ws) || !hasControlMount(ws) {
			t.Fatalf("%+v: want the relay", res)
		}
		if res.calls != 1 {
			t.Fatalf("%+v: docker lookups = %d, want 1 within the retry TTL", res, res.calls)
		}
		*now = now.Add(controlMountRetryTTL)
		hasControlMount(ws)
		if res.calls != 2 {
			t.Fatalf("%+v: docker lookups after the retry TTL = %d, want 2", res, res.calls)
		}
	}
}

func TestHasControlMount_EmptyWorkspaceIsTheRelay(t *testing.T) {
	stubControlMountLookup(t, func(ws string) (bool, bool, error) {
		t.Errorf("unexpected docker lookup for %q", ws)
		return false, false, nil
	})
	if !hasControlMount("") {
		t.Fatal("no workspace: want the relay")
	}
}

func TestConfigMentionsAgent(t *testing.T) {
	write := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"no config", nil, false},
		{"plain config", map[string]string{".devcontainer/devcontainer.json": `{"image":"x"}`}, false},
		{"mounts the agent", map[string]string{".devcontainer/devcontainer.json": `{"mounts":["source=${localEnv:SSH_AUTH_SOCK},target=/ssh-agent,type=bind"]}`}, true},
		{"root-level config", map[string]string{".devcontainer.json": `{"remoteEnv":{"SSH_AUTH_SOCK":"/ssh-agent"}}`}, true},
		{"compose file", map[string]string{".devcontainer/devcontainer.json": `{"dockerComposeFile":"compose.yml"}`, ".devcontainer/compose.yml": "volumes:\n  - ${SSH_AUTH_SOCK}:/ssh-agent\n"}, true},
		{"named config", map[string]string{".devcontainer/py/devcontainer.json": `{"mounts":["source=${localEnv:SSH_AUTH_SOCK},target=/a,type=bind"]}`}, true},
		{"too deep", map[string]string{".devcontainer/a/b/notes.txt": "SSH_AUTH_SOCK"}, false},
		{"root compose file", map[string]string{".devcontainer/devcontainer.json": `{"dockerComposeFile":["../docker-compose.yml"]}`, "docker-compose.yml": "volumes:\n  - ${SSH_AUTH_SOCK}:/ssh-agent\n"}, true},
		{"root compose override", map[string]string{"docker-compose.dev.yaml": "environment:\n  SSH_AUTH_SOCK: /ssh-agent\n"}, true},
		{"root compose.yaml", map[string]string{"compose.yaml": "volumes:\n  - ${SSH_AUTH_SOCK}:/ssh-agent\n"}, true},
		{"root Dockerfile", map[string]string{"Dockerfile": "FROM x\nENV SSH_AUTH_SOCK=/ssh-agent\n"}, true},
		{"root Dockerfile variant", map[string]string{"Dockerfile.dev": "FROM x\nENV SSH_AUTH_SOCK=/ssh-agent\n"}, true},
		{"other root files are not configs", map[string]string{"README.md": "export SSH_AUTH_SOCK=...", "compose.txt": "SSH_AUTH_SOCK"}, false},
		{"plain root compose file", map[string]string{"docker-compose.yml": "services:\n  app:\n    image: x\n"}, false},
		{"oversized file", map[string]string{"Dockerfile": "SSH_AUTH_SOCK" + strings.Repeat(" ", maxConfigBytes)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := t.TempDir()
			for path, content := range c.files {
				write(t, filepath.Join(ws, path), content)
			}
			if got := configMentionsAgent(ws); got != c.want {
				t.Fatalf("configMentionsAgent = %v, want %v", got, c.want)
			}
		})
	}
}

// A symlinked .devcontainer (e.g. into a shared dotfiles checkout) is
// followed; WalkDir alone would not descend into it.
func TestConfigMentionsAgent_SymlinkedDevcontainer(t *testing.T) {
	shared := t.TempDir()
	if err := os.WriteFile(filepath.Join(shared, "devcontainer.json"), []byte(`{"mounts":["source=${localEnv:SSH_AUTH_SOCK},target=/a,type=bind"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.Symlink(shared, filepath.Join(ws, ".devcontainer")); err != nil {
		t.Fatal(err)
	}
	if !configMentionsAgent(ws) {
		t.Fatal("configMentionsAgent = false through a symlinked .devcontainer, want true")
	}
}
