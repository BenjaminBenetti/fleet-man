package codespaces

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
)

// ===========================================
// Backend
// ===========================================

// CodespacesBackend implements backend.Backend using the GitHub CLI (gh).
// The "containerID" for codespaces is the codespace name returned by
// `gh codespace create`.
type CodespacesBackend struct {
	verbose          bool
	repo             string // GitHub repo (owner/repo)
	machine          string // machine type (e.g. "basicLinux32gb")
	idleTimeout      string // idle timeout (e.g. "30m")
	devcontainerPath string // path to devcontainer.json in repo
	branch           string // git branch (empty = repo default)

	// nameCache maps workspaceDir to the real codespace name assigned by
	// GitHub. Populated by Up() so that subsequent ExecCommand calls on
	// the same backend instance use the correct name.
	nameCache map[string]string

	// sshConfigs caches SSH connection info keyed by codespace name.
	// Populated by Up() (generation) or RegisterName() (loading from disk).
	// When present, methods use native `ssh` instead of `gh codespace ssh`
	// for proper PTY allocation.
	sshConfigs map[string]sshConfig
}

// New creates a new CodespacesBackend.
func New(opts ...Option) *CodespacesBackend {
	codespacesBackend := &CodespacesBackend{
		nameCache:  make(map[string]string),
		sshConfigs: make(map[string]sshConfig),
	}
	for _, o := range opts {
		o(codespacesBackend)
	}
	return codespacesBackend
}

// RegisterName associates a workspace dir with its real codespace name.
// This allows ExecCommand and other methods to use the correct name
// when called from contexts that know the container ID (e.g. the TUI).
// Also loads any existing SSH config from disk so that native SSH is
// used for subsequent commands.
func (codespacesBackend *CodespacesBackend) RegisterName(workspaceDir, codespace string) {
	codespacesBackend.nameCache[workspaceDir] = codespace
	codespacesBackend.loadSSHConfig(codespace, workspaceDir)
}

// resolveCodespaceName returns the real codespace name for a workspace dir.
// Checks the cache first (populated by Up or RegisterName), then falls
// back to deriving from the path.
func (codespacesBackend *CodespacesBackend) resolveCodespaceName(workspaceDir string) string {
	if name, ok := codespacesBackend.nameCache[workspaceDir]; ok {
		return name
	}
	return codespaceName(workspaceDir)
}

// ===========================================
// Lifecycle
// ===========================================

// Up creates a GitHub Codespace. workspaceDir is used to derive the display
// name. The repo is set via WithRepo at construction time. The mounts
// argument is ignored — Codespaces are managed by GitHub and the gh CLI
// cannot inject host bind mounts.
func (codespacesBackend *CodespacesBackend) Up(workspaceDir string, _ []backend.Mount) (*backend.UpResult, error) {
	displayName := codespaceName(workspaceDir)

	args := []string{"codespace", "create", "--repo", codespacesBackend.repo, "--display-name", displayName}
	if codespacesBackend.branch != "" {
		args = append(args, "--branch", codespacesBackend.branch)
	}
	if codespacesBackend.machine != "" {
		args = append(args, "--machine", codespacesBackend.machine)
	}
	if codespacesBackend.idleTimeout != "" {
		args = append(args, "--idle-timeout", codespacesBackend.idleTimeout)
	}
	if codespacesBackend.devcontainerPath != "" {
		args = append(args, "--devcontainer-path", codespacesBackend.devcontainerPath)
	}

	cmd := exec.Command("gh", args...)
	// Always tee stderr to os.Stderr so output reaches the log file
	// when run from the TUI background process.
	var stderrBuf strings.Builder
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)

	out, err := cmd.Output()
	if err != nil {
		stderr := stderrBuf.String()
		if isAuthScopeError(stderr) {
			return nil, fmt.Errorf("%s%s", ErrPrefixAuthScope, "gh auth token is missing the \"codespace\" scope")
		}
		if isMachineSelectionError(stderr) {
			return nil, fmt.Errorf("%s%s", ErrPrefixMachine, "machine type required — configure one in Settings > Codespaces")
		}
		if isCodespaceLimitError(stderr) {
			return nil, fmt.Errorf("%s%s", ErrPrefixLimit, "codespace limit reached")
		}
		return nil, fmt.Errorf("gh codespace create failed: %w\nstderr: %s", err, strings.TrimSpace(stderr))
	}

	csName := strings.TrimSpace(string(out))
	if csName == "" {
		return nil, fmt.Errorf("gh codespace create returned empty name")
	}

	// Cache the real name so ExecCommand can find it later.
	codespacesBackend.nameCache[workspaceDir] = csName

	// Wait for the codespace to become available.
	if err := codespacesBackend.waitForState(csName, "Available", 5*time.Minute); err != nil {
		return nil, err
	}

	// Generate SSH config for native SSH access with proper PTY support.
	// Non-fatal: methods fall back to gh codespace ssh if this fails.
	_, _ = codespacesBackend.generateSSHConfig(csName, workspaceDir)

	return &backend.UpResult{
		Outcome:               "success",
		ContainerID:           csName,
		RemoteUser:            "codespace",
		RemoteWorkspaceFolder: "/workspaces",
	}, nil
}

// Down deletes a codespace permanently.
func (codespacesBackend *CodespacesBackend) Down(containerID string) error {
	delete(codespacesBackend.sshConfigs, containerID)
	cmd := exec.Command("gh", "codespace", "delete", "-c", containerID, "--force")
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Stop stops a running codespace.
func (codespacesBackend *CodespacesBackend) Stop(containerID string) error {
	cmd := exec.Command("gh", "codespace", "stop", "-c", containerID)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Start resumes a stopped codespace using the GitHub REST API.
func (codespacesBackend *CodespacesBackend) Start(containerID string) error {
	cmd := exec.Command("gh", "api", "-X", "POST",
		fmt.Sprintf("/user/codespaces/%s/start", containerID))
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh api start codespace: %w", err)
	}

	return codespacesBackend.waitForState(containerID, "Available", 5*time.Minute)
}

// ===========================================
// Execution
// ===========================================

// Exec runs an interactive command inside a codespace via SSH.
func (codespacesBackend *CodespacesBackend) Exec(workspaceDir string, command []string) error {
	name := codespacesBackend.resolveCodespaceName(workspaceDir)
	cmd := codespacesBackend.sshCommand(name, command, true)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ExecCommand returns an unstarted *exec.Cmd for running a command
// inside a codespace via SSH.
func (codespacesBackend *CodespacesBackend) ExecCommand(workspaceDir string, command []string) *exec.Cmd {
	name := codespacesBackend.resolveCodespaceName(workspaceDir)
	return codespacesBackend.sshCommand(name, command, true)
}

// ===========================================
// Monitoring
// ===========================================

// Stats returns CPU and memory usage for the given codespace names.
// Uses SSH to read process stats from each codespace concurrently.
func (codespacesBackend *CodespacesBackend) Stats(containerIDs []string) (map[string]*backend.ContainerStats, error) {
	if len(containerIDs) == 0 {
		return nil, nil
	}

	type statsResult struct {
		id    string
		stats *backend.ContainerStats
		err   error
	}

	ch := make(chan statsResult, len(containerIDs))
	for _, id := range containerIDs {
		go func(csName string) {
			stats, err := codespacesBackend.fetchStats(csName)
			ch <- statsResult{csName, stats, err}
		}(id)
	}

	result := make(map[string]*backend.ContainerStats)
	for range containerIDs {
		received := <-ch
		if received.err == nil && received.stats != nil {
			result[received.id] = received.stats
		}
	}

	return result, nil
}

// Logs streams codespace creation logs.
func (codespacesBackend *CodespacesBackend) Logs(containerID string, follow bool) error {
	args := []string{"codespace", "logs", "-c", containerID}
	if follow {
		args = append(args, "--follow")
	}

	cmd := exec.Command("gh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// LogsCommand returns an unstarted *exec.Cmd for streaming codespace logs.
func (codespacesBackend *CodespacesBackend) LogsCommand(containerID string, follow bool) *exec.Cmd {
	args := []string{"codespace", "logs", "-c", containerID}
	if follow {
		args = append(args, "--follow")
	}
	return exec.Command("gh", args...)
}

// CaptureAllSessions lists and captures every tmux session inside a
// codespace in a single SSH round trip.
func (codespacesBackend *CodespacesBackend) CaptureAllSessions(containerID string) backend.AllSessions {
	cmd := codespacesBackend.sshCommand(containerID, []string{backend.CaptureAllScript}, false)
	out, err := cmd.Output()
	if err != nil {
		return backend.AllSessions{OK: false}
	}
	return backend.AllSessions{Sessions: backend.ParseAllSessionsOutput(string(out)), OK: true}
}

// AgentToolProbe detects which agent tool is running inside a codespace.
func (codespacesBackend *CodespacesBackend) AgentToolProbe(containerID string) (string, bool) {
	cmd := codespacesBackend.sshCommand(containerID, []string{backend.ToolProbeScript}, false)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return backend.ParseToolProbeOutput(string(out))
}

// ===========================================
// Integration
// ===========================================

// EditorURI returns a VS Code URI for connecting to a GitHub Codespace.
func (codespacesBackend *CodespacesBackend) EditorURI(workspaceDir string, projectName string) (string, bool) {
	name := codespacesBackend.resolveCodespaceName(workspaceDir)
	uri := fmt.Sprintf("vscode://github.codespaces/connect?name=%s", name)
	return uri, true
}

// PortForwardCommand returns an unstarted *exec.Cmd that forwards localPort
// on the host to remotePort inside the codespace using `gh codespace ports forward`.
//
// gh expects the mapping as "<remote-port>:<local-port>", which is the
// opposite order from `coder port-forward --tcp=<local>:<remote>`.
func (codespacesBackend *CodespacesBackend) PortForwardCommand(containerID string, localPort, remotePort int) *exec.Cmd {
	mapping := fmt.Sprintf("%d:%d", remotePort, localPort)
	return exec.Command("gh", "codespace", "ports", "forward", mapping, "-c", containerID)
}

// ResolveHostname returns ("", false) for GitHub Codespaces because they
// are remote and not directly reachable by IP from the host.
func (codespacesBackend *CodespacesBackend) ResolveHostname(containerID string) (string, bool) {
	return "", false
}

// SupportsCustomMounts reports false: GitHub Codespaces are provisioned
// by GitHub's control plane and the gh CLI does not expose a way to
// inject host bind mounts.
func (codespacesBackend *CodespacesBackend) SupportsCustomMounts() bool {
	return false
}

// Status reports the live state of a GitHub Codespace via
// `gh codespace view`. GitHub's lifecycle: Available → running;
// Shutdown/Archived → stopped (the most common drift the live probe
// catches: a codespace stopped overnight by the idle timeout while
// fleet still showed it as running). Transitional states like
// Starting/Provisioning/Queued map to unknown so the persisted
// state isn't clobbered while the transition completes.
func (codespacesBackend *CodespacesBackend) Status(containerID string) backend.LiveStatus {
	if containerID == "" {
		return backend.LiveStatusUnknown
	}
	info, err := codespacesBackend.getCodespace(containerID)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := string(exitErr.Stderr)
			if strings.Contains(stderr, "not found") || strings.Contains(stderr, "could not find codespace") {
				return backend.LiveStatusMissing
			}
		}
		return backend.LiveStatusUnknown
	}
	switch info.State {
	case "Available":
		return backend.LiveStatusRunning
	case "Shutdown", "Archived":
		return backend.LiveStatusStopped
	default:
		return backend.LiveStatusUnknown
	}
}

// ===========================================
// Internal helpers
// ===========================================

// getCodespace fetches codespace details via `gh codespace view`.
// The returned error is wrapped with %w so that callers can use
// errors.As to recover the underlying *exec.ExitError (and its
// captured stderr) when they need to distinguish "missing" from
// other failure modes.
func (codespacesBackend *CodespacesBackend) getCodespace(name string) (*codespaceInfo, error) {
	cmd := exec.Command("gh", "codespace", "view", "-c", name, "--json", "name,displayName,state")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh codespace view: %w", err)
	}

	var info codespaceInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parsing codespace view output: %w", err)
	}

	return &info, nil
}

// waitForState polls until the codespace reaches the desired state or times out.
func (codespacesBackend *CodespacesBackend) waitForState(name, desiredState string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		info, err := codespacesBackend.getCodespace(name)
		if err == nil && info.State == desiredState {
			return nil
		}
		time.Sleep(3 * time.Second)
	}

	return fmt.Errorf("codespace %q did not reach state %q within %v", name, desiredState, timeout)
}

// fetchStats reads CPU and memory stats from a codespace via SSH.
func (codespacesBackend *CodespacesBackend) fetchStats(csName string) (*backend.ContainerStats, error) {
	cmd := codespacesBackend.sshCommand(csName, []string{"ps", "-eo", "pcpu=,rss=", "--no-headers"}, false)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var totalCPU, totalMem float64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		cpu, _ := parseFloat(fields[0])
		mem, _ := parseFloat(fields[1])
		totalCPU += cpu
		totalMem += mem
	}

	return &backend.ContainerStats{
		CPUMillicores: totalCPU * 10,
		MemoryMB:      totalMem / 1024,
	}, nil
}
