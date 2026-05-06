// Package localdir implements a Backend that operates directly on a host
// filesystem directory without any container, VM, or remote workspace.
// Lifecycle calls (Stop/Start/Down) are no-ops; Exec runs commands with
// the workspace dir as the working directory; port forwards short-circuit
// to localhost via the in-process TCP proxy.
package localdir

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// ===========================================
// Options
// ===========================================

// Option configures a LocalDirBackend.
type Option func(*LocalDirBackend)

// WithVerbose enables verbose output. Currently a no-op for parity with
// other backends; reserved for future diagnostics.
func WithVerbose(v bool) Option {
	return func(b *LocalDirBackend) { b.verbose = v }
}

// ===========================================
// Backend
// ===========================================

// LocalDirBackend implements backend.Backend for instances that live as
// plain directories on the host filesystem. The "containerID" is set to
// the workspace directory path itself; methods that would normally
// address a container ignore it.
type LocalDirBackend struct {
	verbose bool
}

// New creates a new LocalDirBackend.
func New(opts ...Option) *LocalDirBackend {
	b := &LocalDirBackend{}
	for _, o := range opts {
		o(b)
	}
	return b
}

// ===========================================
// Lifecycle
// ===========================================

// Up validates that the workspace directory exists (the caller is
// expected to have cloned the repo) and returns a synthetic UpResult.
// The ContainerID is set to the workspace dir so downstream code that
// gates on a non-empty ContainerID still treats the instance as valid.
func (b *LocalDirBackend) Up(workspaceDir string) (*backend.UpResult, error) {
	if _, err := os.Stat(workspaceDir); err != nil {
		return nil, fmt.Errorf("local workspace dir not accessible: %w", err)
	}
	user := os.Getenv("USER")
	return &backend.UpResult{
		Outcome:               "success",
		ContainerID:           workspaceDir,
		RemoteUser:            user,
		RemoteWorkspaceFolder: workspaceDir,
	}, nil
}

// Down is a no-op. Workspace directory cleanup is performed by the
// caller after Down returns (the same is true for the other backends).
func (b *LocalDirBackend) Down(containerID string) error { return nil }

// Stop is a no-op. The interface contract requires Stateful()==false
// callers to skip Stop entirely; this implementation is defensive.
func (b *LocalDirBackend) Stop(containerID string) error { return nil }

// Start is a no-op. See Stop.
func (b *LocalDirBackend) Start(containerID string) error { return nil }

// Stateful reports false: local directories have no lifecycle state.
func (b *LocalDirBackend) Stateful() bool { return false }

// SupportsDotfiles reports false: the workspace is the host filesystem,
// where the user's real dotfiles already live. Cloning into ~/dotfiles
// would interfere with the host environment.
func (b *LocalDirBackend) SupportsDotfiles() bool { return false }

// SupportsAgentForwarding reports false: the host's SSH_AUTH_SOCK is
// already directly accessible. Re-pinning the socket would try to
// touch system paths like /run/ssh-agent.sock, which fails without
// privileges and serves no purpose locally.
func (b *LocalDirBackend) SupportsAgentForwarding() bool { return false }

// ===========================================
// Execution
// ===========================================

// Exec runs an interactive command with the workspace dir as cwd.
func (b *LocalDirBackend) Exec(workspaceDir string, command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("exec requires a command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = workspaceDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ExecCommand returns an unstarted *exec.Cmd that will run command with
// the workspace dir as cwd. Returning a benign /bin/true command for an
// empty argv keeps cmd.String() and cmd.Start() safe for callers that
// don't pre-validate.
func (b *LocalDirBackend) ExecCommand(workspaceDir string, command []string) *exec.Cmd {
	var cmd *exec.Cmd
	if len(command) == 0 {
		cmd = exec.Command("true")
	} else {
		cmd = exec.Command(command[0], command[1:]...)
	}
	cmd.Dir = workspaceDir
	return cmd
}

// ===========================================
// Monitoring
// ===========================================

// Stats always reports no data. Surfacing per-instance host process stats
// is left for a future iteration (would require scoping by cwd or pgid).
func (b *LocalDirBackend) Stats(containerIDs []string) (map[string]*backend.ContainerStats, error) {
	return nil, nil
}

// Logs prints a short notice — local instances have no separate runtime
// log stream. The creation log captured by `fleet up` is still available
// through the TUI logs viewer.
func (b *LocalDirBackend) Logs(containerID string, follow bool) error {
	fmt.Println("Local backend has no runtime logs.")
	return nil
}

// LogsCommand returns an `echo` cmd so the TUI's logs script can splice
// it in without nil checks. Using echo (not printf) keeps cmd.String()
// shell-safe when interpolated into a wrapper script.
func (b *LocalDirBackend) LogsCommand(containerID string, follow bool) *exec.Cmd {
	return exec.Command("echo", "Local backend has no runtime logs.")
}

// CaptureAllSessions runs the shared tmux capture script directly on the
// host. The host's tmux server is shared across all local instances, but
// session-name prefixing (sanitized instance name) keeps them disjoint.
// Sessions belonging to unrelated host work may also appear; they will
// simply be ignored by the per-instance session filtering upstream.
func (b *LocalDirBackend) CaptureAllSessions(containerID string) backend.AllSessions {
	cmd := exec.Command("sh", "-c", backend.CaptureAllScript)
	out, err := cmd.Output()
	if err != nil {
		return backend.AllSessions{OK: false}
	}
	return backend.AllSessions{
		Sessions: backend.ParseAllSessionsOutput(string(out)),
		OK:       true,
	}
}

// AgentToolProbe always reports "no agent" for local instances. A host
// `ps` scan would match agents from any unrelated process, so we opt out
// of detection until a scoped probe (cwd or pgid based) is implemented.
func (b *LocalDirBackend) AgentToolProbe(containerID string) (string, bool) {
	return "", true
}

// ===========================================
// Integration
// ===========================================

// EditorURI returns a `file://` URI pointing at the workspace dir, which
// `code --folder-uri` accepts to open the directory in VS Code.
func (b *LocalDirBackend) EditorURI(workspaceDir string, projectName string) (string, bool) {
	u := &url.URL{Scheme: "file", Path: workspaceDir}
	return u.String(), true
}

// PortForwardCommand should not be reached: ResolveHostname returns
// ("localhost", true), which directs the port-forward manager to use
// its in-process TCP proxy. The returned cmd fails loudly if invoked.
func (b *LocalDirBackend) PortForwardCommand(containerID string, localPort, remotePort int) *exec.Cmd {
	return exec.Command("sh", "-c", "echo 'local backend uses in-process port forwarding' >&2; exit 1")
}

// ResolveHostname always returns localhost so the port-forward manager
// can spin up an in-process TCP proxy instead of shelling out.
func (b *LocalDirBackend) ResolveHostname(containerID string) (string, bool) {
	return "localhost", true
}
