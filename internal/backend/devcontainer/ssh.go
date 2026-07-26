package devcontainer

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// containerSSHSocketPath is the target path for the SSH agent socket inside
// managed containers. Uses /run instead of /tmp because some devcontainer
// features (e.g. docker-in-docker) mount a tmpfs on /tmp that shadows bind mounts.
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

// sshAgentSockOverrideEnv overrides the bind source used for SSH agent
// forwarding into instances: a path replaces the computed source verbatim
// (no host-side existence check — the path may only exist inside the Docker
// VM), and "off" or "none" disables the agent mount entirely.
const sshAgentSockOverrideEnv = "FLEET_SSH_AGENT_SOCK"

// hostSSHAuthSock returns the host's SSH_AUTH_SOCK path if the environment
// variable is set and the socket exists. Returns empty string otherwise.
func hostSSHAuthSock() string {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return ""
	}
	info, err := os.Stat(sock)
	if err != nil {
		return ""
	}
	if info.Mode()&os.ModeSocket == 0 {
		return ""
	}
	return sock
}

// agentSockDisabled reports whether an override value is the forwarding
// kill-switch. Case-insensitive so OFF/None from a shell rc don't fall
// through and get bind-mounted verbatim as a (nonexistent) source path.
func agentSockDisabled(override string) bool {
	return strings.EqualFold(override, "off") || strings.EqualFold(override, "none")
}

// sshAgentMountSource returns the bind source to use for the SSH agent
// socket mount, or "" when agent forwarding should be skipped. Shared by
// devcontainer up and clone so every container-creation path applies the
// same gate, darwin remap, and override. An unusable override value is a
// hard error (matching the other FLEET_* env parsers): silently splicing
// it into the mount string would resurface as a confusing docker failure
// far from the cause — commas would even inject extra mount options.
func sshAgentMountSource() (string, error) {
	override := strings.TrimSpace(os.Getenv(sshAgentSockOverrideEnv))
	if override != "" && !agentSockDisabled(override) {
		if strings.Contains(override, ",") || !filepath.IsAbs(override) {
			return "", fmt.Errorf("invalid %s value %q (valid: an absolute socket path, or off/none to disable agent forwarding)", sshAgentSockOverrideEnv, override)
		}
	}
	return sshAgentMountSourceFor(override, hostSSHAuthSock(), runtime.GOOS), nil
}

// sshAgentMountSourceFor is the pure core of sshAgentMountSource, split out
// so both the linux and darwin branches are testable on any platform.
func sshAgentMountSourceFor(override, hostSock, goos string) string {
	switch {
	case agentSockDisabled(override):
		return ""
	case override != "":
		return override
	}
	if hostSock == "" {
		return ""
	}
	// The macOS agent socket only exists on the mac side; the Docker VM
	// exposes the agent at its own fixed path. The hostSock gate still
	// requires an agent to actually be running.
	if goos == "darwin" {
		return dockerDesktopSSHAuthSock
	}
	return hostSock
}

// sshUpArgs returns additional devcontainer up arguments to bind-mount the
// host SSH agent socket and set SSH_AUTH_SOCK inside the container.
// Returns nil args if SSH agent forwarding is not available, and an error
// for an unusable FLEET_SSH_AGENT_SOCK value.
func sshUpArgs() ([]string, error) {
	sock, err := sshAgentMountSource()
	if err != nil {
		return nil, err
	}
	if sock == "" {
		return nil, nil
	}
	return []string{
		"--mount", "type=bind,source=" + sock + ",target=" + containerSSHSocketPath,
		"--remote-env", "SSH_AUTH_SOCK=" + containerSSHSocketPath,
	}, nil
}

// sshExecArgs returns additional devcontainer exec arguments to set
// SSH_AUTH_SOCK inside the container. It uses the same gate as the mount
// (sshAgentMountSource) so exec sessions export the variable exactly when
// the socket is actually mounted — e.g. FLEET_SSH_AGENT_SOCK=off suppresses
// both, and an override path with no host agent enables both. An invalid
// override already hard-fails instance creation; exec just degrades to no
// forwarding rather than blocking shells into an existing container.
func sshExecArgs() []string {
	sock, err := sshAgentMountSource()
	if err != nil || sock == "" {
		return nil
	}
	return []string{
		"--remote-env", "SSH_AUTH_SOCK=" + containerSSHSocketPath,
	}
}

// execArgs builds the full argument list for `devcontainer exec` including
// SSH agent forwarding.
func execArgs(workspaceDir string, command []string) []string {
	args := []string{"exec", "--workspace-folder", workspaceDir}
	args = append(args, sshExecArgs()...)
	args = append(args, command...)
	return args
}
