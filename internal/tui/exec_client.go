package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/execstream"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
	"github.com/BenjaminBenetti/fleet-man/internal/portforward"
)

// exec_client.go is the TUI's seam onto the server's interactive-backend RPCs
// (ResolveExecCommand / ResolveLogsCommand / Forward) and the browser +
// coder RPCs. The TUI no longer constructs a backend (the P5 boundary): the
// server returns the argv it would run and the client execs it locally (the TTY
// carve-out — local exec for attach is a permitted client-host action), or the
// server does the container work itself and returns a result (browser/coder).
//
// The exec resolution + the browser/coder calls reuse the shared mutation
// connection (client.go) so the TUI keeps a single persistent server link. The
// resolution RPCs are bounded by a short timeout; the resolved command the
// caller then runs is NOT (it may be a long-lived shell / tmux attach).
//
// Against a REMOTE daemon (FLEET_GATEWAY / FLEET_SERVER) the carve-out can't
// hold — the resolved argv references binaries and containers on the daemon's
// host — so the same surface streams over the Exec bidi RPC instead:
// runInstanceCommand for one-shots (tmux session management), attachExecCmd
// for interactive attach (a self-exec of `fleet shell`, which streams).

// execResolveTimeout bounds a single resolve RPC (fast — the server just builds
// argv). The command the caller runs afterwards is unbounded.
const execResolveTimeout = 10 * time.Second

// browserPrepareTimeout bounds PrepareBrowser, which may install privoxy +
// stage the landing-page binary inside the container (apt-get etc.), so it is
// generous.
const browserPrepareTimeout = 3 * time.Minute

// coderFetchTimeout bounds the Coder-API template-params fetch (network).
const coderFetchTimeout = 30 * time.Second

// resolveExecArgv asks the server for the argv (+ env) to run argv inside the
// instance. A package var so tests can stub the whole RPC.
var resolveExecArgv = func(fleetName, instanceName string, argv []string) ([]string, map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execResolveTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return nil, nil, err
	}
	reply, err := conn.Service().ResolveExecCommand(ctx, &fleetgrpc.ResolveExecCommandRequest{
		Fleet:    fleetName,
		Instance: instanceName,
		Argv:     argv,
	})
	if err != nil {
		return nil, nil, err
	}
	return reply.GetArgv(), reply.GetEnv(), nil
}

// resolveExecCmd resolves the exec argv for the instance and builds a local
// *exec.Cmd with the server-supplied env merged over the client's. The caller
// sets stdio / reads .Args / runs it (Output/CombinedOutput/Run/tea.Exec).
func resolveExecCmd(fleetName, instanceName string, argv []string) (*exec.Cmd, error) {
	resolved, env, err := resolveExecArgv(fleetName, instanceName, argv)
	if err != nil {
		return nil, err
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("server returned no exec command for %s/%s", fleetName, instanceName)
	}
	c := exec.Command(resolved[0], resolved[1:]...)
	c.Env = mergeExecEnv(env)
	return c, nil
}

// instanceCommandTimeout bounds one non-interactive command run inside an
// instance (the tmux new/rename/kill/list one-shots — fast, but generous
// enough for a remote round-trip).
const instanceCommandTimeout = 30 * time.Second

// runInstanceCommand runs argv inside the instance and returns its stdout and
// exit code. Local daemon: resolve the backend argv and run it locally (the
// carve-out path, same semantics as cmd.Output()). Remote daemon: stream over
// the Exec bidi RPC, since the resolved argv only makes sense on the daemon's
// host. err covers resolve/transport/start failures; a command that ran but
// exited non-zero returns err == nil with its exit code. A package var so
// tests can stub the whole round-trip.
var runInstanceCommand = func(fleetName, instanceName string, argv []string) (stdout string, exitCode int, err error) {
	if fleetclient.IsRemote() {
		ctx, cancel := context.WithTimeout(context.Background(), instanceCommandTimeout)
		defer cancel()
		conn, err := dialMutation(ctx)
		if err != nil {
			return "", 0, err
		}
		out, code, err := execstream.Output(ctx, conn.Service(), fleetName, instanceName, argv)
		return string(out), code, err
	}
	cmd, err := resolveExecCmd(fleetName, instanceName, argv)
	if err != nil {
		return "", 0, err
	}
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode(), nil
	}
	if err != nil {
		return string(out), 0, err
	}
	return string(out), 0, nil
}

// attachExecCmd builds the local *exec.Cmd that attaches the user's terminal
// to argv inside the instance. Local daemon: the server-resolved backend argv
// (the TTY carve-out). Remote daemon: re-invoke this fleet binary's
// `shell <fleet>/<instance> -- argv...`, which streams over the Exec RPC —
// keeping tea.ExecProcess, tmux split panes, and terminal windows working
// unchanged (they still just run a local process). The child inherits
// FLEET_GATEWAY / FLEET_SERVER / FLEET_TOKEN from the environment. A package
// var so tests can stub it.
var attachExecCmd = func(fleetName, instanceName string, argv []string) (*exec.Cmd, error) {
	if !fleetclient.IsRemote() {
		return resolveExecCmd(fleetName, instanceName, argv)
	}
	self, err := os.Executable()
	if err != nil {
		self = "fleet" // fall back to PATH lookup at run time
	}
	args := append([]string{"shell", fleetName + "/" + instanceName, "--"}, argv...)
	return exec.Command(self, args...), nil
}

// mergeExecEnv layers the server-supplied env over the client's own. Nil/empty
// means "inherit" (a nil Cmd.Env makes exec inherit os.Environ()).
func mergeExecEnv(extra map[string]string) []string {
	if len(extra) == 0 {
		return nil
	}
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// forwardDialer returns a TargetDialer that tunnels one connection per call
// over the server's Forward bidi stream (the port-forward data plane) to
// remotePort inside the instance. It reuses the shared mutation connection;
// the bounded ctx covers only the dial — each per-connection stream manages
// its own lifetime.
var forwardDialer = func(fleetName, instanceName string, remotePort int) (portforward.TargetDialer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execResolveTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return nil, err
	}
	return portforward.NewGRPCTarget(conn.Service(), fleetName, instanceName, remotePort), nil
}

// resolveLogsArgv resolves the host-side container-logs command for the
// instance; the logs view embeds it in its pager script.
var resolveLogsArgv = func(fleetName, instanceName string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execResolveTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return nil, err
	}
	reply, err := conn.Service().ResolveLogsCommand(ctx, &fleetgrpc.ResolveLogsCommandRequest{
		Fleet:    fleetName,
		Instance: instanceName,
	})
	if err != nil {
		return nil, err
	}
	return reply.GetArgv(), nil
}

// --- Browser RPCs ----------------------------------------------------------

// fetchBrowserConfig reads the workspace devcontainer.json customization so the
// TUI can decide whether to show the launch chooser. Package var for stubbing.
var fetchBrowserConfig = func(fleetName, instanceName string) (initialURL string, hasLanding bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), execResolveTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return "", false, err
	}
	reply, err := conn.Service().GetBrowserConfig(ctx, &fleetgrpc.GetBrowserConfigRequest{
		Fleet:    fleetName,
		Instance: instanceName,
	})
	if err != nil {
		return "", false, err
	}
	return reply.GetInitialUrl(), reply.GetHasLanding(), nil
}

// prepareBrowserRemote does the container-side browser work (ensure proxy +
// resolve URL, starting the landing page when chosen) and returns the URL the
// local browser should open. Package var for stubbing.
var prepareBrowserRemote = func(fleetName, instanceName string, preferFleetLaunch bool, targetURL string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), browserPrepareTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return "", err
	}
	req := &fleetgrpc.PrepareBrowserRequest{
		Fleet:             fleetName,
		Instance:          instanceName,
		PreferFleetLaunch: preferFleetLaunch,
	}
	if targetURL != "" {
		req.TargetUrl = &targetURL
	}
	reply, err := conn.Service().PrepareBrowser(ctx, req)
	if err != nil {
		return "", err
	}
	return reply.GetInitialUrl(), nil
}

// --- Coder template params -------------------------------------------------

// coderRichParam mirrors the subset of a Coder rich parameter the settings
// dialog renders/stores (replacing the direct coderbackend.RichParameter the
// TUI used before the server owned backend access).
type coderRichParam struct {
	Name         string
	DisplayName  string
	Description  string
	Type         string
	DefaultValue string
}

// getCoderTemplateParamsRemote fetches a template's rich parameters + preset
// names from the server. Package var for stubbing.
var getCoderTemplateParamsRemote = func(template string) ([]coderRichParam, []string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), coderFetchTimeout)
	defer cancel()
	conn, err := dialMutation(ctx)
	if err != nil {
		return nil, nil, err
	}
	reply, err := conn.Service().GetCoderTemplateParams(ctx, &fleetgrpc.GetCoderTemplateParamsRequest{
		Template: template,
	})
	if err != nil {
		return nil, nil, err
	}
	params := make([]coderRichParam, 0, len(reply.GetParameters()))
	for _, p := range reply.GetParameters() {
		params = append(params, coderRichParam{
			Name:         p.GetName(),
			DisplayName:  p.GetDisplayName(),
			Description:  p.GetDescription(),
			Type:         p.GetType(),
			DefaultValue: p.GetDefaultValue(),
		})
	}
	return params, reply.GetPresets(), nil
}
