package backend

import (
	"io"
	"os/exec"
)

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

	// Rebuild tears down the instance's existing container and provisions a
	// fresh one in place, PRESERVING the workspace: the host-side bind mount
	// (devcontainer) or the codespace's persisted code survives, so uncommitted
	// edits are not lost. It is the way to pick up an edited devcontainer.json /
	// Dockerfile without recreating the instance from scratch.
	//
	// containerID identifies the existing container/workspace (the codespace
	// name for the codespaces backend; unused by devcontainer, which matches by
	// workspace-folder label). mounts carry the same caller-supplied bind mounts
	// as Up and must be re-applied so the rebuilt container keeps its control
	// socket / shared-cache / agent-state mounts; backends returning false from
	// SupportsCustomMounts ignore them.
	//
	// Returns the same UpResult shape as Up so callers can persist the new
	// ContainerID (it CHANGES for devcontainer; it is stable for codespaces).
	// Backends that cannot rebuild return false from SupportsRebuild and an
	// error from Rebuild.
	Rebuild(containerID, workspaceDir string, mounts []Mount) (*UpResult, error)

	// SupportsRebuild reports whether this backend implements Rebuild. Callers
	// should check this before offering a rebuild action; managed backends whose
	// runtime exposes no rebuild primitive (coder) report false.
	SupportsRebuild() bool

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

	// CopyFile streams src INTO the instance, writing it to remotePath with the
	// given mode. It is the single strategy seam for host→instance file transfer:
	// callers (the fleet-launch binary stager, the `fleet copy` server handler)
	// invoke it without caring which transport a backend uses. The write is
	// atomic (same-directory temp + rename), creates remotePath's parent, and
	// assumes that parent is writable by the exec user (a caller needing a
	// privileged destination copies to a writable staging path and moves it into
	// place separately).
	//
	// Crucially, an implementation MUST NOT rely on the remote command reading
	// stdin to EOF: the coder backend's SSH transport never half-closes a remote
	// command's stdin, so a `cat > file` there hangs forever. devcontainer (local
	// docker exec) and codespaces (OpenSSH, no PTY) stream over stdin via
	// StreamWriteScript; coder transfers out-of-band with scp.
	//
	// This is the INTO direction (host → instance); it is unrelated to the
	// fleetgrpc CopyFile RPC, which streams a file OUT of an instance.
	CopyFile(workspaceDir string, src io.Reader, remotePath string, mode int) error

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

	// ListSessions lists the live tmux sessions inside a container, returning
	// the raw `tmux list-sessions` output (see ListSessionsScript) for the
	// caller to parse. Unlike ExecCommand/ExecCommandQuiet — which resolve the
	// container from a workspace folder and, on the devcontainer backend, pay a
	// Node CLI cold start per call — this execs directly against the container
	// ID, making it cheap enough to run on the 1s session poll. Returns "" on
	// any exec failure, which the caller treats as "no sessions".
	ListSessions(containerID string) string

	// AgentToolProbe detects which agent tool (if any) is running
	// inside a container. Returns (tool, true) on success,
	// ("", false) on probe failure.
	AgentToolProbe(containerID string) (string, bool)

	// RunScript runs an arbitrary shell script directly inside the container as
	// the session user, returning its combined output. Like
	// ListSessions/CaptureAllSessions it execs straight against the container ID
	// — no devcontainer Node CLI cold start and no workspace-folder resolution —
	// which makes it the reliable path for non-interactive daemon-side container
	// work (e.g. launching an automation agent's tmux session: a session created
	// here lands under the same session user the poller reads, so it shows in the
	// TUI). The error is non-nil when the exec itself fails.
	RunScript(containerID, script string) (string, error)

	// EditorURI returns a URI string that an editor (e.g. VS Code)
	// can use to connect to this workspace. Returns ("", false) if
	// editor integration is not supported by this backend.
	EditorURI(workspaceDir string, projectName string) (string, bool)

	// PortForwardCommand returns an unstarted *exec.Cmd that tunnels
	// traffic from localPort on the host to remotePort inside the
	// container/workspace. The process runs until killed by the caller.
	PortForwardCommand(containerID string, localPort, remotePort int) *exec.Cmd

	// ForwardStdioCommand returns an unstarted *exec.Cmd whose stdin/stdout
	// bridge ONE connection to remotePort inside the container/workspace
	// (no listening socket on the host). Used by the server's Forward data
	// plane, which pipes the process's stdio over a gRPC stream. Returns
	// (nil, false) when the backend has no stdio bridge; callers fall back
	// to PortForwardCommand bound to a host-local port.
	ForwardStdioCommand(containerID string, remotePort int) (*exec.Cmd, bool)

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
