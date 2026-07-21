package devcontainer

import (
	"os"
	"runtime"
)

// containerSSHSocketPath is the target path for the SSH agent socket inside
// managed containers. Uses /run instead of /tmp because some devcontainer
// features (e.g. docker-in-docker) mount a tmpfs on /tmp that shadows bind mounts.
const containerSSHSocketPath = "/run/ssh-agent.sock"

// dockerDesktopSSHAuthSock is the fixed path at which Docker Desktop (and
// OrbStack, which is path-compatible) exposes the host's SSH agent inside its
// Linux VM. On macOS the host's real SSH_AUTH_SOCK (a launchd socket under
// /private/tmp) is not visible to the VM, so bind-mounting it fails with
// "bind source path does not exist" — this VM-side path must be used instead.
const dockerDesktopSSHAuthSock = "/run/host-services/ssh-auth.sock"

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

// sshUpArgs returns additional devcontainer up arguments to bind-mount the
// host SSH agent socket and set SSH_AUTH_SOCK inside the container.
// Returns nil if SSH agent forwarding is not available.
func sshUpArgs() []string {
	sock := hostSSHAuthSock()
	if sock == "" {
		return nil
	}
	// The macOS agent socket only exists on the mac side; the Docker VM
	// exposes the agent at its own fixed path. The host check above still
	// gates on an agent actually running.
	if runtime.GOOS == "darwin" {
		sock = dockerDesktopSSHAuthSock
	}
	return []string{
		"--mount", "type=bind,source=" + sock + ",target=" + containerSSHSocketPath,
		"--remote-env", "SSH_AUTH_SOCK=" + containerSSHSocketPath,
	}
}

// sshExecArgs returns additional devcontainer exec arguments to set
// SSH_AUTH_SOCK inside the container. Returns nil if SSH_AUTH_SOCK
// is not set on the host.
func sshExecArgs() []string {
	if os.Getenv("SSH_AUTH_SOCK") == "" {
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
