package coder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	"github.com/BenjaminBenetti/fleet-man/internal/flog"
)

// CoderBackend implements backend.Backend using the Coder CLI.
// The "containerID" for coder workspaces is the workspace name.
type CoderBackend struct {
	verbose    bool
	template   string            // coder template name
	preset     string            // coder preset name
	parameters map[string]string // resolved parameter key=value pairs
}

// New creates a new CoderBackend.
func New(opts ...Option) *CoderBackend {
	coderBackend := &CoderBackend{}
	for _, opt := range opts {
		opt(coderBackend)
	}
	return coderBackend
}

// Up creates a Coder workspace. workspaceDir is used to derive the workspace
// name (last path component). The git clone happens inside the Coder template
// via the repo parameter. The mounts argument is ignored: Coder workspaces
// are managed remotely and SupportsCustomMounts reports false.
func (coderBackend *CoderBackend) Up(workspaceDir string, _ []backend.Mount) (*backend.UpResult, error) {
	// Derive workspace name from the workspace dir path.
	// workspaceDir format: ~/.fleet/workspaces/{fleet}/{instance}/{fleet}
	// We need a unique, valid coder workspace name.
	name := coderWorkspaceName(workspaceDir)

	args := []string{"create", name, "--yes"}
	if coderBackend.template != "" {
		args = append(args, "--template", coderBackend.template)
	}
	if coderBackend.preset != "" {
		args = append(args, "--preset", coderBackend.preset)
	}
	for k, v := range coderBackend.parameters {
		args = append(args, "--parameter", k+"="+v)
	}

	cmd := exec.Command("coder", args...)
	// Always write to os.Stdout/os.Stderr so output reaches the log
	// file when run from the TUI background process.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("coder create failed: %w", err)
	}

	// Wait for the agent to be connected and its startup script to finish.
	remoteDir, err := coderBackend.waitForAgent(name)
	if err != nil {
		return nil, err
	}

	// Detect and provision nested devcontainer if present.
	coderBackend.maybeDevcontainerUp(name, remoteDir)

	// Wait for the devcontainer agent to register and connect.
	// After devcontainer up finishes there is a brief delay before
	// the coder agent appears as "connected" in the API.
	sshTarget := name
	for i := 0; i < 20; i++ {
		if dcAgent := coderBackend.findDevcontainerAgent(name); dcAgent != "" {
			sshTarget = name + "." + dcAgent
			break
		}
		time.Sleep(3 * time.Second)
	}

	return &backend.UpResult{
		Outcome:               "success",
		ContainerID:           sshTarget,
		RemoteUser:            "coder",
		RemoteWorkspaceFolder: remoteDir,
	}, nil
}

// workspaceName extracts the workspace name from a containerID which may
// be in "workspace.agent" format. Coder lifecycle commands (stop, start,
// delete) operate on the workspace, not individual agents.
func workspaceName(containerID string) string {
	if dotIndex := strings.Index(containerID, "."); dotIndex >= 0 {
		return containerID[:dotIndex]
	}
	return containerID
}

// resolveSSHTarget returns the best SSH target for a workspace. If the
// workspace has a connected devcontainer agent, returns "workspace.agent".
// Otherwise returns the containerID as-is. This ensures SSH always routes
// to the devcontainer when one exists.
func (coderBackend *CoderBackend) resolveSSHTarget(containerID string) string {
	// Already has an agent suffix — use as-is
	if strings.Contains(containerID, ".") {
		return containerID
	}
	if agent := coderBackend.findDevcontainerAgent(containerID); agent != "" {
		return containerID + "." + agent
	}
	return containerID
}

// Down deletes a Coder workspace permanently.
func (coderBackend *CoderBackend) Down(containerID string) error {
	cmd := exec.Command("coder", "delete", "--yes", workspaceName(containerID))
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Stop stops a Coder workspace.
func (coderBackend *CoderBackend) Stop(containerID string) error {
	cmd := exec.Command("coder", "stop", "--yes", workspaceName(containerID))
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Start starts a stopped Coder workspace. If the workspace contains a
// nested devcontainer, it is re-provisioned after the agent is ready.
func (coderBackend *CoderBackend) Start(containerID string) error {
	wsName := workspaceName(containerID)
	cmd := exec.Command("coder", "start", "--yes", wsName)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	// Wait for agent readiness then restart nested devcontainer if present.
	remoteDir, _ := coderBackend.waitForAgent(wsName)
	coderBackend.maybeDevcontainerUp(wsName, remoteDir)
	return nil
}

// Exec runs an interactive command inside a Coder workspace via SSH, logging
// a single timed "container exec" event when it completes.
func (coderBackend *CoderBackend) Exec(workspaceDir string, command []string) error {
	cmd := coderBackend.rawExec(workspaceDir, command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	start := time.Now()
	err := cmd.Run()
	flog.ContainerExec("coder", workspaceDir, command, time.Since(start), err)
	return err
}

// ExecCommand returns an unstarted *backend.Cmd for running a command inside
// a Coder workspace via SSH. Because *Cmd shadows the run methods, it logs a
// single timed "container exec" event when the caller runs it via
// Run/Output/CombinedOutput. Hot polling loops should use ExecCommandQuiet.
func (coderBackend *CoderBackend) ExecCommand(workspaceDir string, command []string) *backend.Cmd {
	return backend.NewCmd(coderBackend.rawExec(workspaceDir, command), func(d time.Duration, err error) {
		flog.ContainerExec("coder", workspaceDir, command, d, err)
	})
}

// ExecCommandQuiet is ExecCommand without any event-log entries.
func (coderBackend *CoderBackend) ExecCommandQuiet(workspaceDir string, command []string) *backend.Cmd {
	return backend.NewCmd(coderBackend.rawExec(workspaceDir, command), nil)
}

// rawExec builds the underlying `coder ssh` *exec.Cmd shared by ExecCommand
// and ExecCommandQuiet.
func (coderBackend *CoderBackend) rawExec(workspaceDir string, command []string) *exec.Cmd {
	name := coderWorkspaceName(workspaceDir)
	target := coderBackend.resolveSSHTarget(name)
	args := sshArgs(target, command)
	return exec.Command("coder", args...)
}

// Stats returns CPU and memory usage for the given workspace IDs (names).
// Uses SSH to read /proc stats from each workspace concurrently.
func (coderBackend *CoderBackend) Stats(containerIDs []string) (map[string]*backend.ContainerStats, error) {
	return backend.ConcurrentStats(containerIDs, coderBackend.fetchWorkspaceStats)
}

// Logs streams workspace build logs.
func (coderBackend *CoderBackend) Logs(containerID string, follow bool) error {
	args := []string{"logs", workspaceName(containerID)}
	if follow {
		args = append(args, "--follow")
	}

	cmd := exec.Command("coder", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// LogsCommand returns an unstarted *exec.Cmd for streaming workspace logs.
func (coderBackend *CoderBackend) LogsCommand(containerID string, follow bool) *exec.Cmd {
	args := []string{"logs", workspaceName(containerID)}
	if follow {
		args = append(args, "--follow")
	}
	return exec.Command("coder", args...)
}

// CaptureAllSessions lists and captures every tmux session inside a
// Coder workspace in a single SSH round trip.
func (coderBackend *CoderBackend) CaptureAllSessions(containerID string) backend.AllSessions {
	target := coderBackend.resolveSSHTarget(containerID)
	cmd := exec.Command("coder", sshArgs(target, []string{backend.CaptureAllScript})...)
	out, err := cmd.Output()
	if err != nil {
		return backend.AllSessions{OK: false}
	}
	sessions, files, hookMissing := backend.ParseCaptureOutput(string(out))
	return backend.AllSessions{
		Sessions:          sessions,
		ExtraFiles:        files,
		ClaudeHookMissing: hookMissing,
		OK:                true,
	}
}

// AgentToolProbe detects which agent tool is running inside a Coder workspace.
func (coderBackend *CoderBackend) AgentToolProbe(containerID string) (string, bool) {
	// coder ssh wraps everything after -- in a shell invocation, so we
	// pass the script directly rather than via sh -c.
	target := coderBackend.resolveSSHTarget(containerID)
	cmd := exec.Command("coder", sshArgs(target, []string{backend.ToolProbeScript})...)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return backend.ParseToolProbeOutput(string(out))
}

// PortForwardCommand returns an unstarted *exec.Cmd that forwards localPort
// on the host to remotePort inside the Coder workspace using `coder port-forward`.
func (coderBackend *CoderBackend) PortForwardCommand(containerID string, localPort, remotePort int) *exec.Cmd {
	target := coderBackend.resolveSSHTarget(containerID)
	mapping := fmt.Sprintf("--tcp=%d:%d", localPort, remotePort)
	return exec.Command("coder", "port-forward", target, mapping)
}

// ResolveHostname returns ("", false) for Coder workspaces because they
// are remote and not directly reachable by IP from the host.
func (coderBackend *CoderBackend) ResolveHostname(containerID string) (string, bool) {
	return "", false
}

// SupportsCustomMounts reports false: Coder workspaces are provisioned
// by a remote control plane and the local CLI cannot inject host bind
// mounts into them.
func (coderBackend *CoderBackend) SupportsCustomMounts() bool {
	return false
}

// SupportsClone reports false: Coder workspaces are cloned at the
// template level by the control plane, not by fleet-man.
func (coderBackend *CoderBackend) SupportsClone() bool {
	return false
}

// Clone is unsupported for Coder workspaces.
func (coderBackend *CoderBackend) Clone(sourceContainerID, destWorkspaceDir string, mounts []backend.Mount) (*backend.UpResult, error) {
	return nil, fmt.Errorf("coder backend does not support cloning")
}

// Status reports the live state of a Coder workspace by reading
// `coder list`'s LatestBuild.Status. Lifecycle: running/started →
// running; stopped/canceled → stopped; transitional builds (starting,
// stopping, pending, deleting) and unrecognized states map to unknown
// so callers preserve persisted state during in-flight transitions.
// A workspace that no longer exists maps to missing.
func (coderBackend *CoderBackend) Status(containerID string) backend.LiveStatus {
	name := workspaceName(containerID)
	if name == "" {
		return backend.LiveStatusUnknown
	}
	workspace, err := coderBackend.getWorkspace(name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return backend.LiveStatusMissing
		}
		return backend.LiveStatusUnknown
	}
	switch workspace.LatestBuild.Status {
	case "running", "started":
		return backend.LiveStatusRunning
	case "stopped", "canceled":
		return backend.LiveStatusStopped
	case "deleted":
		return backend.LiveStatusMissing
	default:
		return backend.LiveStatusUnknown
	}
}

// EditorURI returns a VS Code URI for connecting to a Coder workspace.
func (coderBackend *CoderBackend) EditorURI(workspaceDir string, projectName string) (string, bool) {
	name := coderWorkspaceName(workspaceDir)
	// VS Code Coder extension uses vscode://coder.coder-remote/open?... format
	// but the simpler approach is to use `coder open vscode` which handles it.
	// Return the workspace name so the CLI can use `coder open vscode <name>`.
	uri := "coder-vscode://" + name
	return uri, true
}

// getWorkspace fetches workspace details via `coder list -o json`.
func (coderBackend *CoderBackend) getWorkspace(name string) (*coderWorkspace, error) {
	cmd := exec.Command("coder", "list", "-o", "json", "--search", "name:"+name)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("coder list: %w", err)
	}

	var workspaces []coderWorkspace
	if err := json.Unmarshal(out, &workspaces); err != nil {
		return nil, fmt.Errorf("parsing coder list output: %w", err)
	}

	for i := range workspaces {
		if workspaces[i].Name == name {
			return &workspaces[i], nil
		}
	}

	return nil, fmt.Errorf("workspace %q not found", name)
}

// waitForAgent polls until the coder agent is connected and its startup
// script has finished (lifecycle_state == "ready"). Returns the agent's
// working directory. Times out after 5 minutes.
func (coderBackend *CoderBackend) waitForAgent(wsName string) (string, error) {
	deadline := time.Now().Add(5 * time.Minute)
	remoteDir := "/workspaces"

	for time.Now().Before(deadline) {
		workspace, err := coderBackend.getWorkspace(wsName)
		if err == nil {
			for _, resource := range workspace.LatestBuild.Resources {
				for _, agent := range resource.Agents {
					if agent.ExpandedDirectory != "" {
						remoteDir = agent.ExpandedDirectory
					}
					if agent.Status == "connected" && agent.LifecycleState == "ready" {
						return remoteDir, nil
					}
				}
			}
		}
		time.Sleep(3 * time.Second)
	}

	// Timed out but return what we have — the workspace may still be usable
	return remoteDir, nil
}

// maybeDevcontainerUp checks if a .devcontainer directory exists under the
// workspace folder and runs `devcontainer up` inside the coder workspace if
// found. This handles the common "nested devcontainer" pattern where a coder
// template provisions a VM/pod and the repo inside contains a devcontainer.
//
// Each individual SSH probe has a 15s timeout so a hanging connection
// cannot consume the full retry budget. The overall deadline is 3 minutes.
func (coderBackend *CoderBackend) maybeDevcontainerUp(wsName, remoteDir string) {
	deadline := time.Now().Add(3 * time.Minute)
	const cmdTimeout = 15 * time.Second
	var wsFolder string

	// Retry finding .devcontainer — the repo clone may still be in progress.
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
		findCmd := exec.CommandContext(ctx, "coder", sshArgs(wsName, []string{
			"find", remoteDir, "-maxdepth", "2", "-name", ".devcontainer", "-type", "d", "-print", "-quit",
		})...)
		out, _ := findCmd.Output()
		cancel()

		// coder ssh may mix remote stderr into stdout (e.g. find's
		// "Permission denied" on /workspaces/lost+found). Scan all
		// lines for one that looks like a valid absolute path.
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "/") && strings.HasSuffix(line, "/.devcontainer") {
				wsFolder = strings.TrimSuffix(line, "/.devcontainer")
				break
			}
		}
		if wsFolder != "" {
			break
		}
		time.Sleep(3 * time.Second)
	}

	if wsFolder == "" {
		return
	}

	// Wait for Docker to be ready before running devcontainer up.
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
		check := exec.CommandContext(ctx, "coder", sshArgs(wsName, []string{"docker", "info"})...)
		err := check.Run()
		cancel()
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Run devcontainer up inside the coder workspace (longer timeout — build can take a while).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dcUp := exec.CommandContext(ctx, "coder", sshArgs(wsName, []string{
		"devcontainer", "up", "--workspace-folder", wsFolder,
	})...)
	if coderBackend.verbose {
		dcUp.Stdout = os.Stdout
		dcUp.Stderr = os.Stderr
	}
	_ = dcUp.Run()
}

// findDevcontainerAgent checks if the workspace has a connected devcontainer
// agent (identified by having a non-nil parent_id). Returns the agent name
// or empty string if none found.
func (coderBackend *CoderBackend) findDevcontainerAgent(wsName string) string {
	workspace, err := coderBackend.getWorkspace(wsName)
	if err != nil {
		return ""
	}
	for _, resource := range workspace.LatestBuild.Resources {
		for _, agent := range resource.Agents {
			if agent.ParentID != nil && agent.Status == "connected" {
				return agent.Name
			}
		}
	}
	return ""
}

// fetchWorkspaceStats reads CPU and memory stats from a workspace via SSH.
func (coderBackend *CoderBackend) fetchWorkspaceStats(wsName string) (*backend.ContainerStats, error) {
	// Simple approach: use ps output piped through awk, but run as
	// a single shell command to avoid escaping issues.
	target := coderBackend.resolveSSHTarget(wsName)
	cmd := exec.Command("coder", sshArgs(target, []string{"ps", "-eo", "pcpu=,rss=", "--no-headers"})...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	return backend.AggregatePSStats(out), nil
}

// sshArgs builds the argument list for `coder ssh`.
// coder ssh concatenates everything after -- and passes it to the remote
// shell, so "sh -c 'script'" must be collapsed to just "script" to
// avoid double-shell wrapping.
func sshArgs(wsName string, command []string) []string {
	args := []string{"ssh"}
	// Forward SSH agent when available on the host.
	if os.Getenv("SSH_AUTH_SOCK") != "" {
		args = append(args, "-A")
	}
	args = append(args, wsName)
	if len(command) == 0 {
		return args
	}
	args = append(args, "--")
	// Collapse "sh -c 'script'" into just "script" since coder ssh
	// already wraps in a shell.
	return backend.AppendRemoteCommand(args, command)
}
