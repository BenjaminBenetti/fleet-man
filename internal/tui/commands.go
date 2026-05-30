package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	coderbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/portforward"
	"github.com/BenjaminBenetti/fleet-man/internal/state"

	tea "github.com/charmbracelet/bubbletea"
)

// Messages

type execDoneMsg struct{ err error }

type instanceSpawnedMsg struct {
	fleet    string
	instance string
}

type instanceCreateErrMsg struct {
	fleet    string
	instance string
	err      error
}

type pollCreatingTickMsg struct{}

// forceRepaintTickMsg fires once per second to trigger a full redraw,
// clearing any stale characters left behind by tmux pane resizes.
type forceRepaintTickMsg struct{}

type statsMsg struct {
	stats        map[string]*backend.ContainerStats
	screens      map[string]backend.AllSessions // containerID → per-session captures (multi-session aware)
	probes       map[string]string              // containerID → detected tool name (from ps aux)
	containerIDs []string                       // containers that were probed (for staleness detection)
}

// Commands

func pollCreatingCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		return pollCreatingTickMsg{}
	})
}

// forceRepaintCmd schedules a forceRepaintTickMsg one second from now.
// The handler in app.Update responds by emitting a synthetic
// tea.WindowSizeMsg with the current dimensions, which invalidates
// bubbletea's line cache and causes every line to be rewritten on the
// next flush. This cleans stale characters left behind by tmux pane
// resizes without flicker — no erase-screen escape is written ahead of
// the redraw, so the terminal never sees a blank frame.
func forceRepaintCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg {
		return forceRepaintTickMsg{}
	})
}

func fetchStatsCmd(instanceBackend backend.Backend, ids []string, delay bool) tea.Cmd {
	return func() tea.Msg {
		if delay {
			time.Sleep(3 * time.Second)
		}
		if len(ids) == 0 {
			return statsMsg{}
		}
		stats, _ := instanceBackend.Stats(ids)
		screens := backend.CaptureAllSessionsForAll(instanceBackend, ids)
		probes := backend.AgentToolProbes(instanceBackend, ids)
		return statsMsg{stats: stats, screens: screens, probes: probes, containerIDs: ids}
	}
}

// operationDoneMsg is sent when a background instance operation completes.
type operationDoneMsg struct {
	fleet    string
	instance string
	message  string
	err      error
}

// toggleInstanceCmd runs stop/start in the background.
func toggleInstanceCmd(fleetName, instanceName string) tea.Cmd {
	toggle := toggleInstanceStatus // capture for goroutine
	return func() tea.Msg {
		result, err := toggle(fleetName, instanceName)
		if err != nil {
			return operationDoneMsg{fleetName, instanceName, "", err}
		}
		key := fleetName + "/" + instanceName
		var msg string
		switch result.Status {
		case fleet.StatusStopped:
			msg = fmt.Sprintf("Stopped %s", key)
		case fleet.StatusRunning:
			msg = fmt.Sprintf("Started %s", key)
		default:
			msg = fmt.Sprintf("Instance %s is %s", key, result.Status)
		}
		return operationDoneMsg{fleetName, instanceName, msg, nil}
	}
}

// deleteInstanceCmd runs instance deletion in the background.
func deleteInstanceCmd(instanceBackend backend.Backend, fleetName, instanceName, containerID, wsDir string, pf *portforward.Manager) tea.Cmd {
	return func() tea.Msg {
		pf.RemoveAll(fleetName + "/" + instanceName)
		_ = instanceBackend.Down(containerID)
		if wsDir != "" {
			_ = os.RemoveAll(wsDir)
		}
		st, err := state.Load()
		if err == nil {
			if f, ok := st.Fleets[fleetName]; ok {
				_ = f.RemoveInstance(instanceName)
				_ = state.Save(st)
			}
		}
		key := fleetName + "/" + instanceName
		return operationDoneMsg{fleetName, instanceName, fmt.Sprintf("Removed %s", key), nil}
	}
}

// deleteFleetCmd runs fleet destruction in the background.
func deleteFleetCmd(backends map[fleet.BackendType]backend.Backend, fleetName string, instances []*fleet.Instance, pf *portforward.Manager) tea.Cmd {
	// Snapshot what we need — don't capture model
	type target struct {
		instanceBackend backend.Backend
		name            string
		containerID     string
		workspaceDir    string
	}
	var targets []target
	for _, instance := range instances {
		backendType := instance.Backend
		if backendType == "" {
			backendType = fleet.BackendDevcontainer
		}
		targets = append(targets, target{backends[backendType], instance.Name, instance.ContainerID, instance.WorkspaceDir})
	}
	return func() tea.Msg {
		for _, instanceTarget := range targets {
			pf.RemoveAll(fleetName + "/" + instanceTarget.name)
			if instanceTarget.instanceBackend != nil {
				_ = instanceTarget.instanceBackend.Down(instanceTarget.containerID)
			}
			if instanceTarget.workspaceDir != "" {
				_ = os.RemoveAll(instanceTarget.workspaceDir)
			}
		}
		st, err := state.Load()
		if err == nil {
			delete(st.Fleets, fleetName)
			_ = state.Save(st)
		}
		return operationDoneMsg{fleetName, "", fmt.Sprintf("Removed fleet %s", fleetName), nil}
	}
}

// coderParamsFetchedMsg is sent when template parameter fetching completes.
type coderParamsFetchedMsg struct {
	params  []coderbackend.RichParameter
	presets []coderbackend.Preset
	err     error
}

// fetchCoderParamsCmd fetches template parameters and presets asynchronously.
func fetchCoderParamsCmd(templateName string) tea.Cmd {
	return func() tea.Msg {
		versionID, err := coderbackend.FetchActiveVersionID(templateName)
		if err != nil {
			return coderParamsFetchedMsg{err: err}
		}

		params, err := coderbackend.FetchRichParameters(versionID)
		if err != nil {
			return coderParamsFetchedMsg{err: err}
		}

		presets, _ := coderbackend.FetchPresets(versionID)
		return coderParamsFetchedMsg{params: params, presets: presets}
	}
}

// codespaceMachine holds a machine type name and its human-readable label.
type codespaceMachine struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// codespaceMachinesFetchedMsg is sent when machine type fetching completes.
type codespaceMachinesFetchedMsg struct {
	machines []codespaceMachine
	err      error
}

// fetchCodespaceMachinesCmd fetches available codespace machine types
// for the given repo via the GitHub API.
func fetchCodespaceMachinesCmd(repo string) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("gh", "api", "repos/"+repo+"/codespaces/machines")
		out, err := cmd.Output()
		if err != nil {
			return codespaceMachinesFetchedMsg{err: err}
		}
		var resp struct {
			Machines []codespaceMachine `json:"machines"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			return codespaceMachinesFetchedMsg{err: err}
		}
		return codespaceMachinesFetchedMsg{machines: resp.Machines}
	}
}

// repoFromRemote extracts "owner/repo" from a git remote URL.
func repoFromRemote(remoteURL string) string {
	// SSH format: git@github.com:owner/repo.git
	if strings.Contains(remoteURL, ":") && strings.Contains(remoteURL, "@") {
		parts := strings.SplitN(remoteURL, ":", 2)
		if len(parts) == 2 {
			return strings.TrimSuffix(parts[1], ".git")
		}
	}
	// HTTPS format: https://github.com/owner/repo.git
	remoteURL = strings.TrimSuffix(remoteURL, ".git")
	parts := strings.Split(remoteURL, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return remoteURL
}

// logsCommand returns an *exec.Cmd for viewing instance logs.
// It always shows the creation log file first (devcontainer up output),
// then appends container runtime logs for running instances.
// Output is always followed by a "press Enter" prompt so the user
// has time to read before the TUI redraws.
func logsCommand(instanceBackend backend.Backend, fleetName string, instance *fleet.Instance) *exec.Cmd {
	logPath := filepath.Join(state.FleetDir(), "logs", fleetName+"-"+instance.Name+".log")
	creationLog := fmt.Sprintf("cat %q 2>/dev/null", logPath)

	var inner string
	switch instance.Status {
	case fleet.StatusFailed, fleet.StatusCreating:
		inner = fmt.Sprintf("%s || echo 'No creation log found.'", creationLog)
	default:
		// Show creation log, then container runtime logs.
		logsCmd := instanceBackend.LogsCommand(instance.ContainerID, false)
		inner = fmt.Sprintf(
			"%s; echo; echo '=== Container runtime logs ==='; echo; %s",
			creationLog, logsCmd.String(),
		)
	}

	// Wrap in a shell that pauses after the output.
	script := fmt.Sprintf(`%s; echo; echo "--- Press Enter to return ---"; read _`, inner)
	return exec.Command("sh", "-c", script)
}

// createInstanceCmd spawns the hidden _create-instance subcommand as a
// detached child process to provision an instance asynchronously.
// branch selects the git ref to check out; an empty string uses the
// repository's default branch.
func createInstanceCmd(fleetName, instanceName, remoteURL, branch string, backendType fleet.BackendType) tea.Cmd {
	return func() tea.Msg {
		self, err := os.Executable()
		if err != nil {
			return instanceCreateErrMsg{fleetName, instanceName, fmt.Errorf("os.Executable: %w", err)}
		}

		args := []string{"_create-instance", fleetName, instanceName, remoteURL, "--backend", string(backendType)}
		if branch != "" {
			args = append(args, "--branch", branch)
		}
		cmd := exec.Command(self, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

		// Log output for debugging
		logDir := filepath.Join(state.FleetDir(), "logs")
		_ = os.MkdirAll(logDir, 0755)
		logFile, err := os.Create(filepath.Join(logDir, fleetName+"-"+instanceName+".log"))
		if err == nil {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		}

		if err := cmd.Start(); err != nil {
			return instanceCreateErrMsg{fleetName, instanceName, fmt.Errorf("spawn: %w", err)}
		}

		// Detach: do not call cmd.Wait(). The child runs independently.
		return instanceSpawnedMsg{fleetName, instanceName}
	}
}

// cloneInstanceCmd spawns the hidden _clone-instance subcommand as a
// detached child to clone an instance asynchronously. The destination
// instance record must already exist in state with StatusCloning so
// the TUI can render progress while the docker commit + run runs.
func cloneInstanceCmd(fleetName, srcInstance, destInstance string) tea.Cmd {
	return func() tea.Msg {
		self, err := os.Executable()
		if err != nil {
			return instanceCreateErrMsg{fleetName, destInstance, fmt.Errorf("os.Executable: %w", err)}
		}

		args := []string{"_clone-instance", fleetName, srcInstance, destInstance}
		cmd := exec.Command(self, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

		logDir := filepath.Join(state.FleetDir(), "logs")
		_ = os.MkdirAll(logDir, 0755)
		logFile, err := os.Create(filepath.Join(logDir, fleetName+"-"+destInstance+".log"))
		if err == nil {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		}

		if err := cmd.Start(); err != nil {
			return instanceCreateErrMsg{fleetName, destInstance, fmt.Errorf("spawn: %w", err)}
		}

		return instanceSpawnedMsg{fleetName, destInstance}
	}
}
