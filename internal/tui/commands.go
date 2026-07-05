package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetpaths"
	"github.com/BenjaminBenetti/fleet-man/internal/portforward"

	tea "github.com/charmbracelet/bubbletea"
)

// Messages

type execDoneMsg struct{ err error }

// resumeMsg wraps the message produced by an execProcess callback so the
// central Update handler can re-assert mouse tracking after the child exits.
// inner is the message the original callback returned (may be nil); it is
// dispatched through Update unchanged.
type resumeMsg struct{ inner tea.Msg }

// execProcess wraps tea.ExecProcess to re-enable mouse tracking once the
// child process exits. bubbletea v1.3.10's RestoreTerminal restores
// alt-screen, bracketed-paste and report-focus but NOT mouse mode, while
// ReleaseTerminal unconditionally disables it — so without this every exec
// permanently kills mouse support in the TUI (issue #88). The re-enable is
// threaded through the callback (not batched alongside ExecProcess) so it
// runs strictly after the program resumes, never racing the pause.
func execProcess(c *exec.Cmd, fn tea.ExecCallback) tea.Cmd {
	return tea.ExecProcess(c, func(err error) tea.Msg {
		var inner tea.Msg
		if fn != nil {
			inner = fn(err)
		}
		return resumeMsg{inner: inner}
	})
}

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

// rebuildInstanceCmd recreates an instance's container in place via a server
// job and reports completion. Like start/stop, the TUI flips an optimistic
// in-memory Rebuilding status at the call site for the spinner and reload()s the
// authoritative result on completion.
func rebuildInstanceCmd(fleetName, instanceName string) tea.Cmd {
	return func() tea.Msg {
		if err := rebuildInstanceRemote(fleetName, instanceName); err != nil {
			return operationDoneMsg{fleetName, instanceName, "", err}
		}
		return operationDoneMsg{fleetName, instanceName, fmt.Sprintf("Rebuilt %s/%s", fleetName, instanceName), nil}
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

// daemonRestartedMsg is sent when the settings "Restart daemon" action finishes.
type daemonRestartedMsg struct{ err error }

// restartDaemonCmd stops the local fleet daemon and relaunches it from the TUI's
// current binary (the same mechanic the version handshake uses). It runs on its
// own background context so the restart isn't cancelled mid-flight, and is
// bounded by the drain/spawn timeouts inside RestartLocalServer.
func restartDaemonCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return daemonRestartedMsg{err: fleetclient.RestartLocalServer(ctx)}
	}
}

// coderParamsFetchedMsg is sent when template parameter fetching completes.
// params/presets come from the server's GetCoderTemplateParams RPC (the server
// owns Coder-API access now). fleetName and template identify which fetch this
// result answers so stale results — the user closed the dialog, moved to
// another fleet, or committed a different template while this fetch was in
// flight — are discarded (mirroring homedirDetectedMsg).
type coderParamsFetchedMsg struct {
	fleetName string
	template  string
	params    []coderRichParam
	presets   []string
	err       error
}

// fetchCoderParamsCmd fetches template parameters and presets asynchronously via
// the server.
func fetchCoderParamsCmd(fleetName, templateName string) tea.Cmd {
	return func() tea.Msg {
		params, presets, err := getCoderTemplateParamsRemote(templateName)
		if err != nil {
			return coderParamsFetchedMsg{fleetName: fleetName, template: templateName, err: err}
		}
		return coderParamsFetchedMsg{fleetName: fleetName, template: templateName, params: params, presets: presets}
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
func logsCommand(fleetName string, instance *fleet.Instance) *exec.Cmd {
	logPath := filepath.Join(fleetpaths.Dir(), "logs", fleetName+"-"+instance.Name+".log")
	creationLog := fmt.Sprintf("cat %q 2>/dev/null", logPath)

	var inner string
	switch instance.Status {
	case fleet.StatusFailed, fleet.StatusCreating:
		inner = fmt.Sprintf("%s || echo 'No creation log found.'", creationLog)
	default:
		// Show creation log, then container runtime logs. The runtime-logs argv
		// is resolved by the server (it owns backend access); embed it in the
		// pager script. A resolve failure degrades to just the creation log.
		argv, err := resolveLogsArgv(fleetName, instance.Name)
		if err != nil || len(argv) == 0 {
			inner = fmt.Sprintf("%s || echo 'No creation log found.'", creationLog)
		} else {
			inner = fmt.Sprintf(
				"%s; echo; echo '=== Container runtime logs ==='; echo; %s",
				creationLog, strings.Join(argv, " "),
			)
		}
	}

	// Wrap in a shell that pauses after the output.
	script := fmt.Sprintf(`%s; echo; echo "--- Press Enter to return ---"; read _`, inner)
	return exec.Command("sh", "-c", script)
}

// daemonLogLevel describes one entry in the settings "Logs" selector. grep is an
// extended-regex matching that level and everything above it; an empty grep means
// no filter (All).
type daemonLogLevel struct {
	label string
	grep  string
}

// daemonLogLevels are the selectable minimum levels, in on-screen order: All,
// Error, Warn, Info. fleetd writes slog text records ("level=ERROR" etc.), so
// "and above" is just a wider alternation. The leading space anchors level= to
// the start of the field (it always follows the quoted time value), which makes a
// stray "level=ERROR" inside a message value unlikely to match — a space-separated
// token inside a value still could, but the worst case is one extra line in a
// transient view, not worth a stricter parse.
var daemonLogLevels = []daemonLogLevel{
	{label: "All", grep: ""},
	{label: "Error", grep: ` level=ERROR`},
	{label: "Warn", grep: ` level=(ERROR|WARN)`},
	{label: "Info", grep: ` level=(ERROR|WARN|INFO)`},
}

// daemonLogStreamCommand returns an *exec.Cmd that tails the fleetd event log
// (~/.fleet/fleet.log) live, optionally filtered to a minimum level. It behaves
// like `tail -f`: the user presses Ctrl-C to stop the stream and return to the
// TUI. -F survives a log recreate, -n 200 shows recent context, and grep's
// --line-buffered makes filtered lines appear promptly rather than block-buffered.
func daemonLogStreamCommand(level daemonLogLevel) *exec.Cmd {
	// ~/.fleet/fleet.log. Computed from fleetpaths rather than flog.Path() because
	// the import-boundary lint bars clients (the TUI) from importing the
	// server-owned flog package; the filename mirrors flog's logFileName.
	path := filepath.Join(fleetpaths.Dir(), "fleet.log")
	stream := fmt.Sprintf("tail -n 200 -F %q", path)
	if level.grep != "" {
		stream += fmt.Sprintf(" | grep --line-buffered -E '%s'", level.grep)
	}
	header := fmt.Sprintf("── fleet.log [%s] ─── Ctrl-C to return to fleet ───", level.label)
	return exec.Command("sh", "-c", fmt.Sprintf("printf '%%s\\n\\n' %q; %s", header, stream))
}

// streamInterruptExitCode is the status some shells report when a pipeline is
// killed by SIGINT (128 + SIGINT).
const streamInterruptExitCode = 128 + int(syscall.SIGINT) // 130

// silenceStreamInterrupt nils out the expected Ctrl-C termination of a log-stream
// child so the shared execDoneMsg handler doesn't flash it in the footer as
// "Command error: …"; any other failure passes through. Ctrl-C in a terminal
// delivers SIGINT to the whole foreground group, and the parent sh surfaces that
// two different ways: some shells exit with 128+SIGINT (130), but dash/bash are
// themselves terminated by the signal, which os/exec reports as a signal death
// (ExitCode() == -1) — both are the documented exit, so both are silenced.
func silenceStreamInterrupt(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() == streamInterruptExitCode {
			return nil
		}
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGINT {
			return nil
		}
	}
	return err
}

// daemonLogStreamCmd hands the terminal to a live fleet.log tail (see
// daemonLogStreamCommand) and returns to the TUI when the user ends it. Ctrl-C —
// the documented way to stop the stream — is treated as a clean exit.
func daemonLogStreamCmd(level daemonLogLevel) tea.Cmd {
	return execProcess(daemonLogStreamCommand(level), func(err error) tea.Msg {
		return execDoneMsg{silenceStreamInterrupt(err)}
	})
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
