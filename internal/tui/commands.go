package tui

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/backend"
	coderbackend "github.com/BenjaminBenetti/fleet-man/internal/backend/coder"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/portforward"

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

// operationDoneMsg is sent when a background instance operation completes.
type operationDoneMsg struct {
	fleet    string
	instance string
	message  string
	err      error
}

// startInstanceCmd / stopInstanceCmd run a start/stop job on the server and
// report completion. The server owns the transition (instanceops) and the
// persisted status; the TUI flips an optimistic in-memory status at the call
// site for the spinner and reload()s the authoritative result on completion.
func startInstanceCmd(fleetName, instanceName string) tea.Cmd {
	return func() tea.Msg {
		if err := startInstanceRemote(fleetName, instanceName); err != nil {
			return operationDoneMsg{fleetName, instanceName, "", err}
		}
		return operationDoneMsg{fleetName, instanceName, fmt.Sprintf("Started %s/%s", fleetName, instanceName), nil}
	}
}

func stopInstanceCmd(fleetName, instanceName string) tea.Cmd {
	return func() tea.Msg {
		if err := stopInstanceRemote(fleetName, instanceName); err != nil {
			return operationDoneMsg{fleetName, instanceName, "", err}
		}
		return operationDoneMsg{fleetName, instanceName, fmt.Sprintf("Stopped %s/%s", fleetName, instanceName), nil}
	}
}

// deleteInstanceCmd tears down an instance via a server job. Port forwards are
// the TUI's own (the server doesn't manage them), so they're removed here first.
func deleteInstanceCmd(fleetName, instanceName string, pf *portforward.Manager) tea.Cmd {
	return func() tea.Msg {
		pf.RemoveAll(fleetName + "/" + instanceName)
		if err := destroyInstanceRemote(fleetName, instanceName, false); err != nil {
			return operationDoneMsg{fleetName, instanceName, "", err}
		}
		return operationDoneMsg{fleetName, instanceName, fmt.Sprintf("Removed %s/%s", fleetName, instanceName), nil}
	}
}

// deleteFleetCmd tears down every instance in the fleet plus the fleet record
// via a single server job (destroy_fleet). TUI-owned port forwards are removed
// here first.
func deleteFleetCmd(fleetName string, instances []*fleet.Instance, pf *portforward.Manager) tea.Cmd {
	names := make([]string, len(instances))
	for i, instance := range instances {
		names[i] = instance.Name
	}
	return func() tea.Msg {
		for _, name := range names {
			pf.RemoveAll(fleetName + "/" + name)
		}
		if err := destroyInstanceRemote(fleetName, "", true); err != nil {
			return operationDoneMsg{fleetName, "", "", err}
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
	logPath := filepath.Join(fleetpaths.Dir(), "logs", fleetName+"-"+instance.Name+".log")
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

// createInstanceCmd dispatches a CreateInstance job to the server, which
// pre-creates the StatusCreating record (no client-side state write — the #63
// fix) and provisions in a server-owned goroutine. The cmd returns once the job
// has started; pollCreating + reload track it to running.
func createInstanceCmd(fleetName, instanceName, remoteURL, branch, color string, backendType fleet.BackendType) tea.Cmd {
	return func() tea.Msg {
		if err := createInstanceRemote(fleetName, instanceName, remoteURL, branch, backendType); err != nil {
			return instanceCreateErrMsg{fleetName, instanceName, err}
		}
		// CreateInstance doesn't carry the UI color; once the record exists
		// (the job has started) apply it as instance metadata.
		if color != "" {
			_ = setInstanceMetadataRemote(fleetName, instanceName, nil, &color, nil)
		}
		return instanceSpawnedMsg{fleetName, instanceName}
	}
}

// cloneInstanceCmd dispatches a CloneInstance job. The server copies the
// source's config/backend/tag/color/branch and pre-creates the StatusCloning
// record, then clones in a server-owned goroutine.
func cloneInstanceCmd(fleetName, srcInstance, destInstance string) tea.Cmd {
	return func() tea.Msg {
		if err := cloneInstanceRemote(fleetName, srcInstance, destInstance); err != nil {
			return instanceCreateErrMsg{fleetName, destInstance, err}
		}
		return instanceSpawnedMsg{fleetName, destInstance}
	}
}
