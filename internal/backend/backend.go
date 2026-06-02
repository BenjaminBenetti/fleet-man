package backend

import "os/exec"

// Backend defines the strategy interface for container runtimes.
// Implementations handle provisioning, lifecycle, execution,
// monitoring, and introspection of containerized workspaces.
type Backend interface {
	// Up creates and starts a workspace from a workspace directory.
	// The mounts argument carries optional custom bind mounts requested
	// by the caller (e.g. shared agentic context dirs). Backends that
	// return false from SupportsCustomMounts may ignore mounts entirely;
	// callers should consult SupportsCustomMounts and pass nil when not
	// supported to avoid wasted host-side preparation.
	Up(workspaceDir string, mounts []Mount) (*UpResult, error)

	// SupportsCustomMounts reports whether this backend can honor the
	// mounts argument passed to Up. Backends that do not control the
	// container's filesystem layer (managed cloud workspaces, for example)
	// should return false.
	SupportsCustomMounts() bool

	// Clone snapshots an existing workspace and starts a new one from
	// that snapshot bound to destWorkspaceDir. The clone must preserve
	// state inside the source container (e.g. manually installed
	// packages) — callers rely on this to fan out hand-configured
	// instances. mounts carry the same caller-supplied bind mounts as
	// Up; backends that return false from SupportsCustomMounts may
	// ignore them.
	//
	// Returns the same shape of UpResult as Up so callers can persist
	// ContainerID identically. Backends that cannot clone should
	// return false from SupportsClone and an error from Clone.
	Clone(sourceContainerID, destWorkspaceDir string, mounts []Mount) (*UpResult, error)

	// SupportsClone reports whether this backend implements Clone.
	// Callers should check this before offering a clone action; cloning
	// is unavailable for managed cloud backends whose snapshot
	// primitives live outside fleet-man's control.
	SupportsClone() bool

	// Down stops and removes a container permanently.
	Down(containerID string) error

	// Stop halts a running container without removing it.
	Stop(containerID string) error

	// Start resumes a previously stopped container.
	Start(containerID string) error

	// Exec runs an interactive command inside a running workspace.
	// Stdin, stdout, and stderr are connected to the caller's terminal.
	Exec(workspaceDir string, command []string) error

	// ExecCommand returns an unstarted *Cmd for running a command inside a
	// workspace. The caller controls stdio and lifecycle. Because *Cmd
	// shadows the run methods, it writes a single timed "container exec"
	// entry to the event log (~/.fleet/fleet.log) when the caller runs it via
	// Run/Output/CombinedOutput. Use it for everything except hot polling
	// loops. Callers needing the raw *exec.Cmd reach it via the returned
	// value's embedded .Cmd field (which forgoes the log entry).
	ExecCommand(workspaceDir string, command []string) *Cmd

	// ExecCommandQuiet is ExecCommand without any event-log entries (its
	// *Cmd carries no completion callback). Use it from high-frequency
	// polling and probe loops (e.g. the periodic tmux session discovery)
	// where logging every command would flood the event log. Behaviour is
	// otherwise identical to ExecCommand.
	ExecCommandQuiet(workspaceDir string, command []string) *Cmd

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

	// Status returns the live state of a single container/workspace as
	// reported by the underlying provider. Callers use this to detect
	// external lifecycle changes — e.g. a codespace that stopped due to
	// inactivity, a docker container that crashed, or a coder workspace
	// stopped by an admin.
	//
	// Returns LiveStatusUnknown when the probe fails (network, auth,
	// daemon down). Callers must treat that as inconclusive and preserve
	// any persisted state rather than overwriting it.
	Status(containerID string) LiveStatus
}
