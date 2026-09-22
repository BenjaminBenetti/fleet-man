package devcontainer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/agentsock"
	"github.com/BenjaminBenetti/fleet-man/internal/control"
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

// hostSSHAuthSock returns the agent socket the daemon was started with if it
// is a live socket, "" otherwise. The macOS Docker Desktop path gates on it.
func hostSSHAuthSock() string {
	if sock := agentsock.OriginSock(); agentsock.LiveSocket(sock) {
		return sock
	}
	return ""
}

// agentPlan is how one container reaches the agent: mount is the host (or
// Docker VM) socket to bind-mount at containerSSHSocketPath ("" for none), and
// sock is the SSH_AUTH_SOCK the container's processes get ("" for none).
type agentPlan struct {
	mount string
	sock  string
}

// agentPlanFor is the pure core of currentAgentPlan, split out so every
// platform's branch is testable anywhere. relayUsable is
// agentsock.RelayUsable: whether the relay can back an agent at all.
func agentPlanFor(mode agentsock.Mode, override, hostSock string, relayUsable bool) agentPlan {
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
		// Pointless — and harmful to an in-container `ssh-agent` fallback —
		// when nothing could ever answer on it.
		if !relayUsable {
			return agentPlan{}
		}
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
	return agentPlanFor(mode, override, hostSock, agentsock.RelayUsable()), nil
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
func sshExecArgs(workspaceDir string) []string {
	plan, err := currentAgentPlan()
	if err != nil || plan.sock == "" {
		return nil
	}
	if plan.sock == agentsock.ContainerSocketPath && !hasControlMount(workspaceDir) {
		// An instance created before the control directory was mounted
		// (fleet < #73) has no relay socket, only the agent socket file bind
		// mounted at creation: keep pointing it there until it is rebuilt.
		plan.sock = containerSSHSocketPath
	}
	return []string{"--remote-env", "SSH_AUTH_SOCK=" + plan.sock}
}

// Caching of the docker answer in hasControlMount. exec runs about once a
// second per instance (session polling), so an answer is reused for
// controlMountCacheTTL; an error or a workspace with no container yet is only
// trusted for controlMountRetryTTL.
const (
	controlMountCacheTTL      = 5 * time.Minute
	controlMountRetryTTL      = 10 * time.Second
	controlMountLookupTimeout = 3 * time.Second
)

// containerHasControlMount looks up the container provisioned from
// workspaceDir and reports whether one exists and whether it bind-mounts the
// control directory at control.ContainerMountDir. A package var so tests can
// stand in for docker.
var containerHasControlMount = dockerHasControlMount

// controlMountNow is the clock behind the cache, swapped by tests.
var controlMountNow = time.Now

type controlMountAnswer struct {
	relay   bool
	expires time.Time
}

var controlMountCache = struct {
	sync.Mutex
	answers map[string]controlMountAnswer // key: workspaceDir
}{answers: make(map[string]controlMountAnswer)}

// hasControlMount reports whether the container of the instance whose
// workspace is workspaceDir sees the relay socket, i.e. was provisioned with
// the instance's control directory mounted. The host directory alone proves
// nothing: the daemon creates it for every running instance, including ones
// whose container predates the mount. So provisioning's marker decides when
// present, and otherwise docker is asked (and the answer cached). Anything
// uncertain — no workspace, no container yet (up is about to create one with
// the mount), docker failing — assumes the relay.
func hasControlMount(workspaceDir string) bool {
	if workspaceDir == "" {
		return true
	}
	// The control directory sits next to the workspace:
	// <workspaces>/<fleet>/<instance>/{<workspace>,.control}.
	controlDir := filepath.Join(filepath.Dir(workspaceDir), control.HostDirName)
	if _, err := os.Stat(control.MountMarkerPath(controlDir)); err == nil {
		return true
	}

	now := controlMountNow()
	controlMountCache.Lock()
	answer, ok := controlMountCache.answers[workspaceDir]
	controlMountCache.Unlock()
	if ok && now.Before(answer.expires) {
		return answer.relay
	}

	// Not under the lock: a slow docker must not stall other instances'
	// cached lookups. Concurrent misses for one workspace may both ask.
	found, mounted, err := containerHasControlMount(workspaceDir)
	answer = controlMountAnswer{relay: true, expires: now.Add(controlMountRetryTTL)}
	if err == nil && found {
		answer = controlMountAnswer{relay: mounted, expires: now.Add(controlMountCacheTTL)}
	}

	controlMountCache.Lock()
	for dir, a := range controlMountCache.answers {
		if !now.Before(a.expires) {
			delete(controlMountCache.answers, dir)
		}
	}
	controlMountCache.answers[workspaceDir] = answer
	controlMountCache.Unlock()
	return answer.relay
}

// dockerHasControlMount is containerHasControlMount against the docker CLI:
// the newest container labelled with the workspace folder (-a, so a stopped
// container about to be started is judged by its own mounts), and whether any
// of its mounts lands exactly at control.ContainerMountDir.
func dockerHasControlMount(workspaceDir string) (found, mounted bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), controlMountLookupTimeout)
	defer cancel()
	out, err := dockerOutput(ctx, "ps", "-a", "-q", "--no-trunc",
		"--filter", "label="+devcontainerLocalFolderLabel+"="+workspaceDir)
	if err != nil {
		return false, false, fmt.Errorf("docker ps: %w", err)
	}
	id, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if id = strings.TrimSpace(id); id == "" {
		return false, false, nil
	}
	out, err = dockerOutput(ctx, "inspect", "-f", "{{range .Mounts}}{{println .Destination}}{{end}}", id)
	if err != nil {
		return true, false, fmt.Errorf("docker inspect %s: %w", id, err)
	}
	for _, dest := range strings.Split(out, "\n") {
		if strings.TrimSpace(dest) == control.ContainerMountDir {
			return true, true, nil
		}
	}
	return true, false, nil
}

// dockerOutput runs one bounded docker CLI call and returns its stdout.
func dockerOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return string(out), err
}

// execArgs builds the full argument list for `devcontainer exec` including
// SSH agent forwarding.
func execArgs(workspaceDir string, command []string) []string {
	args := []string{"exec", "--workspace-folder", workspaceDir}
	args = append(args, sshExecArgs(workspaceDir)...)
	args = append(args, command...)
	return args
}

// maxConfigBytes caps each file configMentionsAgent reads; anything larger is
// not a config file.
const maxConfigBytes = 1 << 20

// rootConfigPatterns are the workspace-root files a devcontainer config
// commonly pulls in from outside .devcontainer (e.g.
// "dockerComposeFile": ["../docker-compose.yml"], "dockerfile": "../Dockerfile").
var rootConfigPatterns = []string{
	"docker-compose*.yml", "docker-compose*.yaml",
	"compose*.yml", "compose*.yaml",
	"Dockerfile*",
}

// configMentionsAgent reports whether the workspace's devcontainer config
// (or a compose file or Dockerfile it builds from) refers to SSH_AUTH_SOCK — a
// project that mounts the host agent itself. Its ${localEnv:SSH_AUTH_SOCK}
// must then resolve to the user's real agent rather than the relay's socket:
// a bind mount pins the socket file, and the relay's is recreated on every
// daemon restart. Anything else about `devcontainer up` (initializeCommand,
// a BuildKit --ssh build) keeps the relay, which dials per connection.
//
// Scanned: .devcontainer.json; compose files and Dockerfiles at the workspace
// root; and .devcontainer/ plus its immediate subdirectories (named configs),
// following a symlinked .devcontainer.
func configMentionsAgent(workspaceDir string) bool {
	if fileMentionsAgent(filepath.Join(workspaceDir, ".devcontainer.json")) {
		return true
	}
	if entries, err := os.ReadDir(workspaceDir); err == nil {
		for _, e := range entries {
			if matchesAny(e.Name(), rootConfigPatterns) && fileMentionsAgent(filepath.Join(workspaceDir, e.Name())) {
				return true
			}
		}
	}
	// The trailing separator makes WalkDir resolve a symlinked .devcontainer
	// to its directory (it does not follow a symlinked root otherwise).
	root := filepath.Join(workspaceDir, ".devcontainer") + string(filepath.Separator)
	found := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if rel, _ := filepath.Rel(root, path); strings.Count(rel, string(filepath.Separator)) >= 1 {
				return filepath.SkipDir // .devcontainer/<name>/ is as deep as configs go
			}
			return nil
		}
		if fileMentionsAgent(path) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// fileMentionsAgent reports whether path is a regular file (after symlinks) of
// at most maxConfigBytes that contains SSH_AUTH_SOCK.
func fileMentionsAgent(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxConfigBytes {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), "SSH_AUTH_SOCK")
}

// matchesAny reports whether name matches one of the filepath.Match patterns.
func matchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
}
