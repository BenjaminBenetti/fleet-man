package devcontainer

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// DevcontainerBackend implements backend.Backend using the devcontainer
// and docker CLIs.
type DevcontainerBackend struct {
	verbose bool

	userCache   map[string]string
	userCacheMu sync.Mutex
}

// New creates a new DevcontainerBackend.
func New(opts ...Option) *DevcontainerBackend {
	devcontainerBackend := &DevcontainerBackend{userCache: make(map[string]string)}
	for _, opt := range opts {
		opt(devcontainerBackend)
	}
	return devcontainerBackend
}

// containerUser returns the non-root user inside the container, caching
// the result to avoid a docker exec round-trip on every poll.
func (devcontainerBackend *DevcontainerBackend) containerUser(containerID string) string {
	devcontainerBackend.userCacheMu.Lock()
	if cached, ok := devcontainerBackend.userCache[containerID]; ok {
		devcontainerBackend.userCacheMu.Unlock()
		return cached
	}
	devcontainerBackend.userCacheMu.Unlock()

	// Pick the owner of an active tmux server socket directory.
	// tmux creates /tmp/tmux-$UID/ for each user that runs a server,
	// so this is the canonical signal for "which user holds fleet's
	// sessions". The [1-9]* glob skips /tmp/tmux-0/ — a stale dir
	// some images leave behind from a root-side init poke at tmux —
	// because fleet's sessions never live under root. Fall back to
	// the first /home/<user>/ owner when no tmux socket exists yet
	// (brand-new container, first poll before any session).
	cmd := exec.Command("docker", "exec", containerID, "sh", "-c",
		`for d in /tmp/tmux-[1-9]*/; do [ -d "$d" ] && stat -c %U "$d" 2>/dev/null && exit; done; `+
			`for d in /home/*/; do [ -d "$d" ] && stat -c %U "$d" 2>/dev/null && exit; done`)
	out, err := cmd.Output()
	user := ""
	if err == nil {
		user = strings.TrimSpace(string(out))
	}

	devcontainerBackend.userCacheMu.Lock()
	devcontainerBackend.userCache[containerID] = user
	devcontainerBackend.userCacheMu.Unlock()
	return user
}

// Up runs `devcontainer up` for the given workspace folder. Any mounts
// in the slice are translated into `--mount type=bind,source=L,target=C`
// flags so they appear inside the container alongside the SSH agent
// socket and the workspace itself.
//
// When the workspace's devcontainer.json declares its own mount at the
// same container path as one of the fleet-supplied mounts, the user's
// entry is temporarily stripped from the file before launching the
// devcontainer CLI and restored once Up returns. Without this, docker
// rejects the resulting `docker run` invocation with "Duplicate mount
// point" and the instance enters StatusFailed. Fleet's mount wins by
// design — the user's per-fleet agent state should override whatever
// the repo's devcontainer.json was pointing at.
func (devcontainerBackend *DevcontainerBackend) Up(workspaceDir string, mounts []backend.Mount) (*backend.UpResult, error) {
	restore, err := neutralizeConflictingMounts(workspaceDir, mounts)
	if err != nil {
		return nil, err
	}
	defer restore()

	args := []string{"up", "--workspace-folder", workspaceDir}
	args, err = devcontainerUpArgs(args)
	if err != nil {
		return nil, err
	}
	args = append(args, sshUpArgs()...)
	args = append(args, customMountArgs(mounts)...)
	cmd := exec.Command("devcontainer", args...)
	env, err := devcontainerEnv(os.Environ())
	if err != nil {
		return nil, err
	}
	cmd.Env = env

	// Capture stdout for JSON parsing, tee to os.Stdout so it appears
	// in log files when run from the TUI background process.
	var stdoutBuf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &stdoutBuf)

	// Always tee stderr to os.Stderr so build output reaches the log
	// file. A buffer captures it for inclusion in error messages.
	var stderrBuf strings.Builder
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderrBuf.String())
		if detail != "" {
			return nil, fmt.Errorf("devcontainer up failed: %w\n%s", err, detail)
		}
		return nil, fmt.Errorf("devcontainer up failed: %w", err)
	}

	var result backend.UpResult
	if err := json.Unmarshal(stdoutBuf.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("failed to parse devcontainer up output: %w\nraw: %s", err, stdoutBuf.String())
	}

	if result.Outcome != "success" {
		return nil, fmt.Errorf("devcontainer up returned outcome %q", result.Outcome)
	}

	return &result, nil
}

// devcontainerUpArgs appends the optional --update-remote-user-uid-default
// flag based on the FLEET_DEVCONTAINER_UPDATE_REMOTE_USER_UID env var.
func devcontainerUpArgs(base []string) ([]string, error) {
	mode := strings.TrimSpace(os.Getenv("FLEET_DEVCONTAINER_UPDATE_REMOTE_USER_UID"))
	switch mode {
	case "", "default":
		return base, nil
	case "never", "on", "off":
		return append(base, "--update-remote-user-uid-default", mode), nil
	default:
		return nil, fmt.Errorf("invalid FLEET_DEVCONTAINER_UPDATE_REMOTE_USER_UID value %q (valid: default, never, on, off)", mode)
	}
}

// devcontainerEnv applies BuildKit-related env tweaks based on the
// FLEET_DEVCONTAINER_BUILDKIT env var.
func devcontainerEnv(base []string) ([]string, error) {
	mode := strings.TrimSpace(os.Getenv("FLEET_DEVCONTAINER_BUILDKIT"))
	switch mode {
	case "", "auto":
		return base, nil
	case "never":
		return append(base, "DOCKER_BUILDKIT=0"), nil
	default:
		return nil, fmt.Errorf("invalid FLEET_DEVCONTAINER_BUILDKIT value %q (valid: auto, never)", mode)
	}
}

// Exec runs `devcontainer exec` in the given workspace folder.
func (devcontainerBackend *DevcontainerBackend) Exec(workspaceDir string, command []string) error {
	args := execArgs(workspaceDir, command)
	cmd := exec.Command("devcontainer", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ExecCommand returns an unstarted *exec.Cmd for running a command
// inside a workspace via `devcontainer exec`.
func (devcontainerBackend *DevcontainerBackend) ExecCommand(workspaceDir string, command []string) *exec.Cmd {
	args := execArgs(workspaceDir, command)
	return exec.Command("devcontainer", args...)
}

// Down stops and removes a container by ID using docker directly.
func (devcontainerBackend *DevcontainerBackend) Down(containerID string) error {
	cmd := exec.Command("docker", "rm", "-f", containerID)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Stop stops a container by ID without removing it.
func (devcontainerBackend *DevcontainerBackend) Stop(containerID string) error {
	cmd := exec.Command("docker", "stop", containerID)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Start starts an existing container by ID.
func (devcontainerBackend *DevcontainerBackend) Start(containerID string) error {
	cmd := exec.Command("docker", "start", containerID)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Stats returns CPU and memory usage for the given container IDs.
func (devcontainerBackend *DevcontainerBackend) Stats(containerIDs []string) (map[string]*backend.ContainerStats, error) {
	if len(containerIDs) == 0 {
		return nil, nil
	}

	args := append([]string{"stats", "--no-stream", "--format", "{{.ID}}\t{{.CPUPerc}}\t{{.MemUsage}}"}, containerIDs...)
	cmd := exec.Command("docker", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	result := make(map[string]*backend.ContainerStats)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 {
			continue
		}

		id := fields[0]
		cpuStr := strings.TrimSuffix(strings.TrimSpace(fields[1]), "%")
		cpuPct, _ := parseFloat(cpuStr)
		cpuMillicores := cpuPct * 10

		memParts := strings.SplitN(fields[2], "/", 2)
		memMB := parseMemToMB(strings.TrimSpace(memParts[0]))

		for _, cid := range containerIDs {
			if strings.HasPrefix(cid, id) || strings.HasPrefix(id, cid) {
				result[cid] = &backend.ContainerStats{
					CPUMillicores: cpuMillicores,
					MemoryMB:      memMB,
				}
			}
		}
	}

	return result, nil
}

// Logs streams docker logs for a container.
func (devcontainerBackend *DevcontainerBackend) Logs(containerID string, follow bool) error {
	args := []string{"logs"}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, containerID)

	cmd := exec.Command("docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// LogsCommand returns an unstarted *exec.Cmd for streaming container logs.
func (devcontainerBackend *DevcontainerBackend) LogsCommand(containerID string, follow bool) *exec.Cmd {
	args := []string{"logs"}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, containerID)
	return exec.Command("docker", args...)
}

// CaptureAllSessions lists and captures every tmux session inside a
// container in one docker exec call.
func (devcontainerBackend *DevcontainerBackend) CaptureAllSessions(containerID string) backend.AllSessions {
	args := []string{"exec"}
	if user := devcontainerBackend.containerUser(containerID); user != "" {
		args = append(args, "-u", user)
	}
	args = append(args, containerID, "sh", "-c", backend.CaptureAllScript)
	cmd := exec.Command("docker", args...)
	out, err := cmd.Output()
	if err != nil {
		return backend.AllSessions{OK: false}
	}
	sessions, files := backend.ParseCaptureOutput(string(out))
	return backend.AllSessions{Sessions: sessions, ExtraFiles: files, OK: true}
}

// AgentToolProbe detects which agent tool is running inside a container.
func (devcontainerBackend *DevcontainerBackend) AgentToolProbe(containerID string) (string, bool) {
	args := []string{"exec"}
	if user := devcontainerBackend.containerUser(containerID); user != "" {
		args = append(args, "-u", user)
	}
	args = append(args, containerID, "sh", "-c", backend.ToolProbeScript)
	cmd := exec.Command("docker", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return backend.ParseToolProbeOutput(string(out))
}

// SupportsCustomMounts reports that the devcontainer backend honors
// caller-supplied bind mounts via `devcontainer up --mount` flags.
func (devcontainerBackend *DevcontainerBackend) SupportsCustomMounts() bool {
	return true
}

// customMountArgs translates a slice of backend.Mount values into the
// `--mount type=bind,...` flag pairs accepted by `devcontainer up`.
// Returns nil for an empty input so callers can append unconditionally.
func customMountArgs(mounts []backend.Mount) []string {
	if len(mounts) == 0 {
		return nil
	}
	args := make([]string, 0, len(mounts)*2)
	for _, mount := range mounts {
		if mount.LocalPath == "" || mount.ContainerPath == "" {
			continue
		}
		args = append(args, "--mount", "type=bind,source="+mount.LocalPath+",target="+mount.ContainerPath)
	}
	return args
}

// EditorURI returns a VS Code devcontainer remote URI.
func (devcontainerBackend *DevcontainerBackend) EditorURI(workspaceDir string, projectName string) (string, bool) {
	hexPath := hex.EncodeToString([]byte(workspaceDir))
	uri := fmt.Sprintf("vscode-remote://dev-container+%s/workspaces/%s", hexPath, projectName)
	return uri, true
}

// PortForwardCommand returns an unstarted *exec.Cmd that forwards localPort
// on the host to remotePort inside the container using socat via docker exec.
func (devcontainerBackend *DevcontainerBackend) PortForwardCommand(containerID string, localPort, remotePort int) *exec.Cmd {
	script := fmt.Sprintf(
		`socat TCP-LISTEN:%d,fork,reuseaddr EXEC:"docker exec -i %s socat STDIO TCP\\:localhost\\:%d"`,
		localPort, containerID, remotePort,
	)
	return exec.Command("sh", "-c", script)
}

// Status reports the live state of a docker container by reading
// `docker inspect --format {{.State.Status}}`. Maps docker's
// fine-grained states into the coarser backend.LiveStatus enum:
// running/restarting → running; exited/paused/dead/created → stopped;
// "No such container" → missing; everything else (and probe errors)
// → unknown so callers preserve whatever was persisted.
func (devcontainerBackend *DevcontainerBackend) Status(containerID string) backend.LiveStatus {
	if containerID == "" {
		return backend.LiveStatusUnknown
	}
	cmd := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", containerID)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if strings.Contains(string(exitErr.Stderr), "No such container") {
				return backend.LiveStatusMissing
			}
		}
		return backend.LiveStatusUnknown
	}
	switch strings.TrimSpace(string(out)) {
	case "running", "restarting":
		return backend.LiveStatusRunning
	case "exited", "paused", "dead", "created":
		return backend.LiveStatusStopped
	case "removing":
		return backend.LiveStatusMissing
	default:
		return backend.LiveStatusUnknown
	}
}

// ResolveHostname returns the container's IP address obtained via
// `docker inspect`. For local Docker containers this is directly reachable.
func (devcontainerBackend *DevcontainerBackend) ResolveHostname(containerID string) (string, bool) {
	out, err := exec.Command("docker", "inspect",
		"--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
		containerID,
	).Output()
	if err != nil {
		return "", false
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", false
	}
	return ip, true
}
