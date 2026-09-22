package devcontainer

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
)

// containerSSHSocketPath is where a DIRECTLY bind-mounted agent socket lands
// inside managed containers (the FLEET_SSH_AGENT_SOCK override and the macOS
// Docker Desktop socket; on Linux instances use the daemon's relay socket
// instead, agentsock.ContainerSocketPath). Uses /run instead of /tmp because
// some devcontainer features (e.g. docker-in-docker) mount a tmpfs on /tmp that
// shadows bind mounts.
const containerSSHSocketPath = "/run/ssh-agent.sock"

// dockerDesktopSSHAuthSock is the fixed path at which Docker Desktop (and
// OrbStack, which is path-compatible; likewise `colima --ssh-agent`) exposes
// the host's SSH agent inside its Linux VM. On macOS the host's real
// SSH_AUTH_SOCK (a launchd socket under /private/tmp) is not visible to the
// VM, so bind-mounting it fails with "bind source path does not exist" — this
// VM-side path must be used instead. Two caveats, both escapable via
// FLEET_SSH_AGENT_SOCK: default Colima, Podman machine, and Rancher Desktop
// don't provide this path; and it forwards the agent Docker Desktop itself
// sees (the default launchd one), which may differ from a custom
// SSH_AUTH_SOCK agent such as 1Password's.
const dockerDesktopSSHAuthSock = "/run/host-services/ssh-auth.sock"

// sshAgentSockOverrideEnv overrides how instances reach the agent: a path is
// bind-mounted verbatim in place of the relay (no host-side existence check —
// the path may only exist inside the Docker VM), and "off" or "none" disables
// agent forwarding entirely.
const sshAgentSockOverrideEnv = agentsock.EnvOverride

// liveSocket returns sock if it names an existing unix socket, "" otherwise.
func liveSocket(sock string) string {
	if sock == "" {
		return ""
	}
	info, err := os.Stat(sock)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return ""
	}
	return sock
}

// hostSSHAuthSock returns the agent socket the daemon was started with if it
// is a live socket, "" otherwise. Only the macOS Docker Desktop path gates on
// it: the relay resolves the agent per connection, so it needs no gate.
func hostSSHAuthSock() string {
	return liveSocket(agentsock.OriginSock())
}

// agentPlan is how one container reaches the agent: mount is the host (or
// Docker VM) socket to bind-mount at containerSSHSocketPath ("" for none), and
// sock is the SSH_AUTH_SOCK the container's processes get ("" for none).
type agentPlan struct {
	mount string
	sock  string
}

// agentPlanFor is the pure core of currentAgentPlan, split out so every
// platform's branch is testable anywhere.
func agentPlanFor(mode agentsock.Mode, override, hostSock string) agentPlan {
	switch mode {
	case agentsock.ModeOff:
		return agentPlan{}
	case agentsock.ModeOverride:
		return agentPlan{mount: override, sock: containerSSHSocketPath}
	case agentsock.ModeDockerDesktop:
		// The hostSock gate requires an agent to actually be running.
		if hostSock == "" {
			return agentPlan{}
		}
		return agentPlan{mount: dockerDesktopSSHAuthSock, sock: containerSSHSocketPath}
	default:
		// The relay: the daemon listens in the instance's control directory,
		// which provisioning already bind-mounts as a DIRECTORY, so nothing is
		// mounted here and a recreated socket reaches running containers.
		return agentPlan{sock: agentsock.ContainerSocketPath}
	}
}

// currentAgentPlan decides the agent plan from the environment. Shared by
// devcontainer up, clone and exec so every path applies the same rules. An
// unusable override value is a hard error (matching the other FLEET_* env
// parsers): silently splicing it into the mount string would resurface as a
// confusing docker failure far from the cause — commas would even inject extra
// mount options.
func currentAgentPlan() (agentPlan, error) {
	override := agentsock.Override()
	mode := agentsock.ModeFor(override, runtime.GOOS)
	if mode == agentsock.ModeOverride && (strings.Contains(override, ",") || !filepath.IsAbs(override)) {
		return agentPlan{}, fmt.Errorf("invalid %s value %q (valid: an absolute socket path, or off/none to disable agent forwarding)", sshAgentSockOverrideEnv, override)
	}
	var hostSock string
	if mode == agentsock.ModeDockerDesktop {
		hostSock = hostSSHAuthSock()
	}
	return agentPlanFor(mode, override, hostSock), nil
}

// sshAgentMountSource returns the socket to bind-mount at
// containerSSHSocketPath, or "" when nothing is mounted (the relay, or no
// forwarding). Used by clone, which builds its own docker run.
func sshAgentMountSource() (string, error) {
	plan, err := currentAgentPlan()
	return plan.mount, err
}

// sshUpArgs returns additional devcontainer up arguments: the agent socket
// mount (override / Docker Desktop only) and SSH_AUTH_SOCK for the lifecycle
// commands. Returns nil args when agent forwarding is off, and an error for an
// unusable FLEET_SSH_AGENT_SOCK value.
func sshUpArgs() ([]string, error) {
	plan, err := currentAgentPlan()
	if err != nil {
		return nil, err
	}
	var args []string
	if plan.mount != "" {
		args = append(args, "--mount", "type=bind,source="+plan.mount+",target="+containerSSHSocketPath)
	}
	if plan.sock != "" {
		args = append(args, "--remote-env", "SSH_AUTH_SOCK="+plan.sock)
	}
	return args, nil
}

// sshExecArgs returns additional devcontainer exec arguments to set
// SSH_AUTH_SOCK inside the container, under the same plan as the mount — so
// FLEET_SSH_AGENT_SOCK=off suppresses both. An invalid override already
// hard-fails instance creation; exec just degrades to no forwarding rather
// than blocking shells into an existing container.
func sshExecArgs() []string {
	plan, err := currentAgentPlan()
	if err != nil || plan.sock == "" {
		return nil
	}
	return []string{"--remote-env", "SSH_AUTH_SOCK=" + plan.sock}
}

// execArgs builds the full argument list for `devcontainer exec` including
// SSH agent forwarding.
func execArgs(workspaceDir string, command []string) []string {
	args := []string{"exec", "--workspace-folder", workspaceDir}
	args = append(args, sshExecArgs()...)
	args = append(args, command...)
	return args
}
