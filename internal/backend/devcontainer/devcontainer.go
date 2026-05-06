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

// Option configures a DevcontainerBackend.
type Option func(*DevcontainerBackend)

// WithVerbose enables verbose output (sends devcontainer stderr to os.Stderr).
func WithVerbose(v bool) Option {
	return func(b *DevcontainerBackend) { b.verbose = v }
}

// DevcontainerBackend implements backend.Backend using the devcontainer
// and docker CLIs.
type DevcontainerBackend struct {
	verbose bool

	userCache   map[string]string
	userCacheMu sync.Mutex
}

// New creates a new DevcontainerBackend.
func New(opts ...Option) *DevcontainerBackend {
	b := &DevcontainerBackend{userCache: make(map[string]string)}
	for _, o := range opts {
		o(b)
	}
	return b
}

// containerUser returns the non-root user inside the container, caching
// the result to avoid a docker exec round-trip on every poll.
func (b *DevcontainerBackend) containerUser(containerID string) string {
	b.userCacheMu.Lock()
	if u, ok := b.userCache[containerID]; ok {
		b.userCacheMu.Unlock()
		return u
	}
	b.userCacheMu.Unlock()

	cmd := exec.Command("docker", "exec", containerID, "sh", "-c",
		`for d in /home/*/; do [ -d "$d" ] && stat -c %U "$d" 2>/dev/null && exit; done`)
	out, err := cmd.Output()
	u := ""
	if err == nil {
		u = strings.TrimSpace(string(out))
	}

	b.userCacheMu.Lock()
	b.userCache[containerID] = u
	b.userCacheMu.Unlock()
	return u
}

// Up runs `devcontainer up` for the given workspace folder.
func (b *DevcontainerBackend) Up(workspaceDir string) (*backend.UpResult, error) {
	args := []string{"up", "--workspace-folder", workspaceDir}
	args = append(args, sshUpArgs()...)
	cmd := exec.Command("devcontainer", args...)

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

// Exec runs `devcontainer exec` in the given workspace folder.
func (b *DevcontainerBackend) Exec(workspaceDir string, command []string) error {
	args := execArgs(workspaceDir, command)
	cmd := exec.Command("devcontainer", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ExecCommand returns an unstarted *exec.Cmd for running a command
// inside a workspace via `devcontainer exec`.
func (b *DevcontainerBackend) ExecCommand(workspaceDir string, command []string) *exec.Cmd {
	args := execArgs(workspaceDir, command)
	return exec.Command("devcontainer", args...)
}

// Down stops and removes a container by ID using docker directly.
func (b *DevcontainerBackend) Down(containerID string) error {
	cmd := exec.Command("docker", "rm", "-f", containerID)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Stop stops a container by ID without removing it.
func (b *DevcontainerBackend) Stop(containerID string) error {
	cmd := exec.Command("docker", "stop", containerID)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Start starts an existing container by ID.
func (b *DevcontainerBackend) Start(containerID string) error {
	cmd := exec.Command("docker", "start", containerID)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Stats returns CPU and memory usage for the given container IDs.
func (b *DevcontainerBackend) Stats(containerIDs []string) (map[string]*backend.ContainerStats, error) {
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
func (b *DevcontainerBackend) Logs(containerID string, follow bool) error {
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
func (b *DevcontainerBackend) LogsCommand(containerID string, follow bool) *exec.Cmd {
	args := []string{"logs"}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, containerID)
	return exec.Command("docker", args...)
}

// CaptureAllSessions lists and captures every tmux session inside a
// container in one docker exec call.
func (b *DevcontainerBackend) CaptureAllSessions(containerID string) backend.AllSessions {
	args := []string{"exec"}
	if u := b.containerUser(containerID); u != "" {
		args = append(args, "-u", u)
	}
	args = append(args, containerID, "sh", "-c", backend.CaptureAllScript)
	cmd := exec.Command("docker", args...)
	out, err := cmd.Output()
	if err != nil {
		return backend.AllSessions{OK: false}
	}
	return backend.AllSessions{Sessions: backend.ParseAllSessionsOutput(string(out)), OK: true}
}

// AgentToolProbe detects which agent tool is running inside a container.
func (b *DevcontainerBackend) AgentToolProbe(containerID string) (string, bool) {
	args := []string{"exec"}
	if u := b.containerUser(containerID); u != "" {
		args = append(args, "-u", u)
	}
	args = append(args, containerID, "sh", "-c", backend.ToolProbeScript)
	cmd := exec.Command("docker", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return backend.ParseToolProbeOutput(string(out))
}

// EditorURI returns a VS Code devcontainer remote URI.
func (b *DevcontainerBackend) EditorURI(workspaceDir string, projectName string) (string, bool) {
	hexPath := hex.EncodeToString([]byte(workspaceDir))
	uri := fmt.Sprintf("vscode-remote://dev-container+%s/workspaces/%s", hexPath, projectName)
	return uri, true
}

// PortForwardCommand returns an unstarted *exec.Cmd that forwards localPort
// on the host to remotePort inside the container using socat via docker exec.
func (b *DevcontainerBackend) PortForwardCommand(containerID string, localPort, remotePort int) *exec.Cmd {
	script := fmt.Sprintf(
		`socat TCP-LISTEN:%d,fork,reuseaddr EXEC:"docker exec -i %s socat STDIO TCP\\:localhost\\:%d"`,
		localPort, containerID, remotePort,
	)
	return exec.Command("sh", "-c", script)
}

// Stateful reports that devcontainers have lifecycle state (running/stopped).
func (b *DevcontainerBackend) Stateful() bool { return true }

// SupportsDotfiles reports that devcontainer instances should receive
// dotfiles, since each container is an isolated environment.
func (b *DevcontainerBackend) SupportsDotfiles() bool { return true }

// ResolveHostname returns the container's IP address obtained via
// `docker inspect`. For local Docker containers this is directly reachable.
func (b *DevcontainerBackend) ResolveHostname(containerID string) (string, bool) {
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
