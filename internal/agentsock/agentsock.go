// Package agentsock names the daemon's SSH-agent relay sockets and decides how
// an instance reaches the user's ssh-agent. It is shared by the daemon, which
// serves the sockets (internal/server), and the devcontainer backend, which
// points instances at them.
//
// The relay exists so the agent an instance or a host-side git clone uses is
// resolved PER CONNECTION rather than frozen when something was created:
//
//   - The daemon listens on a host socket (HostSocketPath) and points its own
//     SSH_AUTH_SOCK at it, so every child it runs (git clone, repo inspection,
//     coder/codespaces ssh -A, devcontainer initializeCommand) goes through it.
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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/BenjaminBenetti/fleet-man/internal/control"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
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
)

// SetRelayServing records that this process serves the relay sockets.
func SetRelayServing(on bool) { relayServing.Store(on) }

// SetRemoteClients records whether remote clients — the only ones that
// provide their agent — can reach this daemon (Remote Fleet enabled).
func SetRemoteClients(on bool) { remoteClients.Store(on) }

// RelayUsable reports whether an instance pointed at the relay can get an
// agent at all: the relay is up, and either the daemon has an agent of its
// own to fall back to or a remote client could attach one. When it cannot,
// instances are not given an SSH_AUTH_SOCK that could only ever fail — a
// dotfiles `[ -z "$SSH_AUTH_SOCK" ] && eval "$(ssh-agent)"` must still work.
func RelayUsable() bool {
	return relayServing.Load() && (remoteClients.Load() || LiveSocket(OriginSock()))
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
