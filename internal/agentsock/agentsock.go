// Package agentsock names the daemon's SSH-agent relay sockets and decides how
// an instance reaches the user's ssh-agent. It is shared by the daemon, which
// serves the sockets (internal/server), and the devcontainer backend, which
// points instances at them.
//
// The relay exists so the agent an instance or a host-side git clone uses is
// resolved PER CONNECTION rather than frozen when something was created:
//
//   - The daemon listens on a host socket (HostSocketPath) and — once
//     something can answer there — points its own SSH_AUTH_SOCK at it, so
//     every child it runs (git clone, repo inspection, coder/codespaces
//     ssh -A, devcontainer initializeCommand unless the project mounts
//     ${localEnv:SSH_AUTH_SOCK} itself) goes through it.
//   - On Linux it also listens inside every devcontainer instance's control
//     directory, which is already bind-mounted into the instance, so the
//     instance sees the socket at ContainerSocketPath. A directory mount
//     survives the socket being recreated; bind-mounting a socket FILE pins
//     its inode and breaks the instance's agent on any reconnect.
//
// Each connection goes to the newest provider (a client streaming its local
// agent over the SSHAgent RPC) that can serve it, else to the agent the daemon
// was started with.
package agentsock

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/gitutil"
)

const (
	// EnvAuthSock is the standard ssh-agent socket variable.
	EnvAuthSock = "SSH_AUTH_SOCK"
	// EnvOverride overrides agent forwarding into instances: a path replaces
	// the relay with a direct bind mount of that path (it may only exist inside
	// the Docker VM), and "off"/"none" disables agent forwarding entirely —
	// the relay included.
	EnvOverride = "FLEET_SSH_AGENT_SOCK"
	// EnvOrigin preserves the SSH_AUTH_SOCK the daemon was started with once it
	// has pointed its own SSH_AUTH_SOCK at the relay (set even when empty, so
	// "started without an agent" survives too).
	EnvOrigin = "FLEET_ORIGIN_SSH_AUTH_SOCK"
	// SocketName is the relay socket's basename inside an instance's control
	// directory (next to control.SocketName).
	SocketName = "ssh-agent.sock"
	// ContainerSocketPath is where an instance sees its relay socket.
	ContainerSocketPath = control.ContainerMountDir + "/" + SocketName
)

// Mode is how instances reach the agent.
type Mode int

const (
	// ModeRelay: instances dial the daemon's per-instance relay socket.
	ModeRelay Mode = iota
	// ModeOff: FLEET_SSH_AGENT_SOCK=off — no agent in instances, no relay.
	ModeOff
	// ModeOverride: FLEET_SSH_AGENT_SOCK=<path> — bind-mount that path as is.
	ModeOverride
	// ModeDockerDesktop: macOS — bind-mount the fixed socket Docker Desktop
	// (and OrbStack, colima --ssh-agent) exposes inside its VM. A host unix
	// socket cannot cross into that VM, so the relay cannot serve instances
	// there; the host relay still serves the daemon's own git.
	ModeDockerDesktop
)

// Override returns the trimmed FLEET_SSH_AGENT_SOCK value.
func Override() string {
	return strings.TrimSpace(os.Getenv(EnvOverride))
}

// Disabled reports whether an override value is the forwarding kill switch.
// Case-insensitive so OFF/None from a shell rc don't fall through and get
// used verbatim as a (nonexistent) socket path.
func Disabled(override string) bool {
	return strings.EqualFold(override, "off") || strings.EqualFold(override, "none")
}

// CurrentMode is ModeFor for this process's environment and platform.
func CurrentMode() Mode {
	return ModeFor(Override(), runtime.GOOS)
}

// ModeFor is the pure core of CurrentMode.
func ModeFor(override, goos string) Mode {
	switch {
	case Disabled(override):
		return ModeOff
	case override != "":
		return ModeOverride
	case goos == "darwin":
		return ModeDockerDesktop
	default:
		return ModeRelay
	}
}

// HostSocketPath is the daemon's host-side relay socket, in a 0700 directory
// under ~/.fleet.
func HostSocketPath() string {
	return filepath.Join(fleetpaths.Dir(), "ssh-agent", "agent.sock")
}

// OriginSock is the agent socket the daemon was started with: SSH_AUTH_SOCK,
// unless that is the daemon's own relay (the daemon redirected it, or this
// process was spawned by one of the daemon's children), in which case the
// preserved EnvOrigin. It never returns the relay socket itself, which would
// make the relay dial itself.
func OriginSock() string {
	sock := os.Getenv(EnvAuthSock)
	if sock == HostSocketPath() {
		return notRelay(os.Getenv(EnvOrigin))
	}
	return sock
}

func notRelay(sock string) string {
	if sock == HostSocketPath() {
		return ""
	}
	return sock
}

// LiveSocket reports whether path names an existing unix socket.
func LiveSocket(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

// Signals from the daemon about what can back the relay; the devcontainer
// backend reads them to decide whether pointing an instance at the relay can
// ever get it an agent.
var (
	relayServing  atomic.Bool
	remoteClients atomic.Bool
	providerSeen  atomic.Bool
	// forwarded: a client is attached to provide its agent right now (whether
	// that agent can serve is only known per connection).
	forwarded atomic.Bool
)

// SetForwarded records whether any client is attached as a provider.
func SetForwarded(on bool) { forwarded.Store(on) }

// CloneForwarding is what agent forwarding could offer a clone this process
// runs now, for gitutil.CloneFailureHint.
func CloneForwarding() gitutil.AgentForwarding {
	switch {
	case CurrentMode() == ModeOff:
		return gitutil.AgentForwardingOff
	case forwarded.Load():
		return gitutil.AgentForwarded
	default:
		return gitutil.AgentNotForwarded
	}
}

// SetRelayServing records that this process serves the relay sockets. Turning
// it off withdraws the published verdict.
func SetRelayServing(on bool) {
	relayServing.Store(on)
	if !on {
		_ = os.Remove(usablePath())
		return
	}
	publishUsable()
}

// SetRemoteClients records whether remote clients can reach this daemon
// (Remote Fleet enabled).
func SetRemoteClients(on bool) { remoteClients.Store(on); publishUsable() }

// SetProviderSeen records that a client has provided its agent to this daemon
// at least once (persisted by the daemon, see ProviderSeenPath): Remote Fleet
// being on only makes a provider POSSIBLE, and a host nobody forwards to must
// keep instances without an SSH_AUTH_SOCK that could only ever fail.
func SetProviderSeen(on bool) { providerSeen.Store(on); publishUsable() }

// ProviderSeenPath marks, across daemon restarts, that a provider attached.
func ProviderSeenPath() string {
	return filepath.Join(filepath.Dir(HostSocketPath()), "provider-seen")
}

// usablePath is where the daemon publishes RelayUsable for processes that run
// a backend in-process (`fleet start`) and so cannot see its signals. It holds
// the daemon's pid: a daemon that was killed cannot withdraw its verdict, so a
// reader only believes one whose daemon is still running.
func usablePath() string {
	return filepath.Join(filepath.Dir(HostSocketPath()), "usable")
}

// RelayUsable reports whether an instance pointed at the relay can get an
// agent at all: the relay is up, and either the daemon has an agent of its
// own to fall back to or a remote client has provided one (and still can).
// When it cannot, instances are not given an SSH_AUTH_SOCK that could only
// ever fail — a dotfiles `[ -z "$SSH_AUTH_SOCK" ] && eval "$(ssh-agent)"`
// must still work. Outside the daemon it reads the daemon's published verdict.
func RelayUsable() bool {
	if !relayServing.Load() {
		return publishedUsable()
	}
	return relayUsable()
}

// publishedUsable reads the verdict a running daemon published.
func publishedUsable() bool {
	data, err := os.ReadFile(usablePath())
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(data)))
	return err == nil && pid > 0 && processAlive(pid)
}

// processAlive reports whether pid names a running process (one owned by
// another user still counts).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func relayUsable() bool {
	return relayServing.Load() && (LiveSocket(OriginSock()) || (remoteClients.Load() && providerSeen.Load()))
}

// RefreshUsable re-publishes the verdict (the daemon's own agent may have
// come or gone since).
func RefreshUsable() { publishUsable() }

// publishUsable mirrors the daemon's verdict to usablePath (best-effort).
func publishUsable() {
	if !relayServing.Load() {
		return
	}
	if relayUsable() {
		// Rewritten only when it changes: this runs on every reconcile tick.
		pid := []byte(strconv.Itoa(os.Getpid()))
		if cur, err := os.ReadFile(usablePath()); err != nil || !bytes.Equal(cur, pid) {
			_ = atomicfile.Write(usablePath(), pid, 0o600)
		}
		return
	}
	_ = os.Remove(usablePath())
}

// WithOriginAgent returns environ with SSH_AUTH_SOCK set back to the agent the
// daemon was started with, when the daemon redirected it to the relay and
// that agent is live. For `devcontainer up`: a config may bind-mount
// ${localEnv:SSH_AUTH_SOCK}, and a bind mount pins the socket FILE — the
// relay's is recreated on every daemon restart, the user's own agent is not.
func WithOriginAgent(environ []string) []string {
	origin := OriginSock()
	if !LiveSocket(origin) {
		return environ
	}
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if strings.HasPrefix(kv, EnvAuthSock+"=") {
			if strings.TrimPrefix(kv, EnvAuthSock+"=") == HostSocketPath() {
				kv = EnvAuthSock + "=" + origin
			}
		}
		out = append(out, kv)
	}
	return out
}
