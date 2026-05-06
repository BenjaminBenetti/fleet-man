package backend

import "os/exec"

// Backend defines the strategy interface for container runtimes.
// Implementations handle provisioning, lifecycle, execution,
// monitoring, and introspection of containerized workspaces.
type Backend interface {
	// Up creates and starts a workspace from a workspace directory.
	Up(workspaceDir string) (*UpResult, error)

	// Down stops and removes a container permanently.
	Down(containerID string) error

	// Stop halts a running container without removing it.
	Stop(containerID string) error

	// Start resumes a previously stopped container.
	Start(containerID string) error

	// Exec runs an interactive command inside a running workspace.
	// Stdin, stdout, and stderr are connected to the caller's terminal.
	Exec(workspaceDir string, command []string) error

	// ExecCommand returns an unstarted *exec.Cmd for running a command
	// inside a workspace. The caller controls stdio and lifecycle.
	ExecCommand(workspaceDir string, command []string) *exec.Cmd

	// Stats returns CPU and memory usage for the given container IDs.
	Stats(containerIDs []string) (map[string]*ContainerStats, error)

	// Logs streams container logs to os.Stdout/os.Stderr synchronously.
	Logs(containerID string, follow bool) error

	// LogsCommand returns an unstarted *exec.Cmd for streaming logs.
	LogsCommand(containerID string, follow bool) *exec.Cmd

	// CaptureAllSessions lists every tmux session inside a container
	// and captures each pane's visible content in a single shell
	// invocation. Used to detect agent activity across all sessions
	// without knowing their names in advance.
	CaptureAllSessions(containerID string) AllSessions

	// AgentToolProbe detects which agent tool (if any) is running
	// inside a container. Returns (tool, true) on success,
	// ("", false) on probe failure.
	AgentToolProbe(containerID string) (string, bool)

	// EditorURI returns a URI string that an editor (e.g. VS Code)
	// can use to connect to this workspace. Returns ("", false) if
	// editor integration is not supported by this backend.
	EditorURI(workspaceDir string, projectName string) (string, bool)

	// PortForwardCommand returns an unstarted *exec.Cmd that tunnels
	// traffic from localPort on the host to remotePort inside the
	// container/workspace. The process runs until killed by the caller.
	PortForwardCommand(containerID string, localPort, remotePort int) *exec.Cmd

	// ResolveHostname returns a hostname or IP address that is directly
	// reachable from the host for the given container/workspace. When
	// the second return value is true, callers can open direct TCP
	// connections to hostname:port without spawning external processes.
	// Returns ("", false) when the container is not directly reachable.
	ResolveHostname(containerID string) (string, bool)

	// Stateful reports whether the backend has lifecycle state, i.e.
	// whether Stop and Start are meaningful. Backends that return false
	// describe instances that are always "running" — callers must not
	// invoke Stop/Start and must not transition instance status.
	Stateful() bool

	// SupportsDotfiles reports whether dotfiles installation should be
	// attempted for this backend's instances. False when the workspace is
	// the host filesystem (where the user's real dotfiles already live and
	// cloning into ~/dotfiles would interfere with the host environment).
	SupportsDotfiles() bool

	// SupportsAgentForwarding reports whether the SSH-agent socket needs
	// to be re-pinned inside the workspace for stability across reattach.
	// True for backends that cross an SSH/exec boundary (devcontainer,
	// coder, codespaces) where each connection mints a fresh socket path.
	// False for host-side backends (local_dir) where the user's real SSH
	// agent is already directly accessible — attempting the relink would
	// fail on system paths like /run/ssh-agent.sock.
	SupportsAgentForwarding() bool
}
